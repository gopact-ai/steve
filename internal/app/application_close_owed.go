package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
)

type applicationSessionCloser interface {
	CloseSession(context.Context, harness.Placement, string) error
}

// applicationOwedCloses sends the closes a conversation let go of while the
// node running the session could not be reached.
type applicationOwedCloses struct {
	mu       sync.Mutex
	store    *state.Store
	attempts *attempt.Service
	tasks    *task.Store
	sessions applicationSessionCloser
	after    string
	// reported holds, per close still owed, why it was last kept, so each
	// cause is logged once rather than on every pass.
	reported map[string]string
}

func newApplicationOwedCloses(store *state.Store, attempts *attempt.Service, tasks *task.Store, sessions applicationSessionCloser) *applicationOwedCloses {
	return &applicationOwedCloses{store: store, attempts: attempts, tasks: tasks, sessions: sessions, reported: map[string]string{}}
}

// owedCloseOutcome says why an obligation remains, or why it can be dropped.
type owedCloseOutcome struct {
	owed    state.OwedClose
	kept    error
	dropped string
}

// Reconcile sends each close still owed. A small rotating batch keeps an
// offline node from starving the closes owed by others. Network calls and
// the settlement proposal share a deadline; committed writes retain the
// ledger's local confirmation semantics. A refused close stays owed for
// the next pass, and why is logged once per cause.
func (c *applicationOwedCloses) Reconcile(parent context.Context) error {
	if !c.mu.TryLock() {
		return nil
	}
	defer c.mu.Unlock()
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	owed, err := c.store.OwedClosesContext(ctx)
	if err != nil {
		return err
	}
	current := make(map[string]bool, len(owed))
	for _, o := range owed {
		current[owedCloseKey(o)] = true
	}
	for key := range c.reported {
		if !current[key] {
			delete(c.reported, key)
		}
	}
	if len(owed) == 0 {
		return nil
	}
	sort.Slice(owed, func(i, j int) bool { return owedCloseKey(owed[i]) < owedCloseKey(owed[j]) })
	start := sort.Search(len(owed), func(i int) bool { return owedCloseKey(owed[i]) > c.after })
	if start == len(owed) {
		start = 0
	}
	count := min(len(owed), 4)
	outcomes := make(chan owedCloseOutcome, count)
	for index := range count {
		o := owed[(start+index)%len(owed)]
		c.after = owedCloseKey(o)
		go func() { outcomes <- c.send(ctx, o) }()
	}
	var settled []state.OwedClose
	var completed []owedCloseOutcome
	for range count {
		outcome := <-outcomes
		key := owedCloseKey(outcome.owed)
		if outcome.kept == nil {
			settled = append(settled, outcome.owed)
			completed = append(completed, outcome)
			continue
		}
		if cause := outcome.kept.Error(); c.reported[key] != cause {
			c.reported[key] = cause
			o := outcome.owed
			slog.Warn(fmt.Sprintf("steve: close owed task=%s attempt=%s node=%s session=%s stays owed: %v", o.TaskID, o.AttemptID, o.NodeID, o.UpstreamID, outcome.kept),
				"task", o.TaskID, "attempt", o.AttemptID, "node", o.NodeID, "session", o.UpstreamID)
		}
	}
	if len(settled) == 0 {
		return ctx.Err()
	}
	if err := c.store.SettleOwedCloses(ctx, settled...); err != nil {
		return fmt.Errorf("settle closes owed: %w", err)
	}
	for _, outcome := range completed {
		delete(c.reported, owedCloseKey(outcome.owed))
		logSettledClose(outcome)
	}
	return nil
}

// send sends owed to its node, bound to the execution it was owed for. It
// is sent only while that is still the latest execution in the session and
// its task is on record; once either is no longer so, nothing could ever
// send the close rightly, and it is forgotten unsent.
func (c *applicationOwedCloses) send(parent context.Context, owed state.OwedClose) owedCloseOutcome {
	ctx, cancel := context.WithTimeout(parent, 17*time.Second)
	defer cancel()
	outcome := owedCloseOutcome{owed: owed}
	latest, found, err := c.attempts.LatestForSession(ctx, owed.NodeID, owed.HarnessID, owed.UpstreamID)
	if err != nil {
		outcome.kept = fmt.Errorf("read the execution in the session: %w", err)
		return outcome
	}
	if !found || latest.ID != owed.AttemptID || latest.TaskID != owed.TaskID || latest.NativeContext != owed.NativeContext {
		outcome.dropped = "the session no longer holds the execution it was owed for"
		return outcome
	}
	if _, ok := c.tasks.Get(owed.TaskID); !ok {
		outcome.dropped = "its task is gone"
		return outcome
	}
	ctx = execution.WithProbeKey(ctx, execution.Key{TaskID: latest.TaskID, InstanceID: latest.TurnID, AttemptID: latest.ID})
	if err := c.sessions.CloseSession(ctx, harness.Placement{Node: owed.NodeID, Harness: owed.HarnessID}, owed.UpstreamID); err != nil {
		var refusal interface{ SessionErrorCode() string }
		if errors.As(err, &refusal) && (refusal.SessionErrorCode() == "conflict" || refusal.SessionErrorCode() == "absent") {
			outcome.dropped = "the node refused: " + refusal.SessionErrorCode()
		} else {
			outcome.kept = err
		}
		return outcome
	}
	return outcome
}

func logSettledClose(outcome owedCloseOutcome) {
	owed := outcome.owed
	if outcome.dropped != "" {
		slog.Warn(fmt.Sprintf("steve: close owed task=%s attempt=%s node=%s session=%s dropped unsent: %s", owed.TaskID, owed.AttemptID, owed.NodeID, owed.UpstreamID, outcome.dropped),
			"task", owed.TaskID, "attempt", owed.AttemptID, "node", owed.NodeID, "session", owed.UpstreamID)
		return
	}
	slog.Info(fmt.Sprintf("steve: close owed task=%s attempt=%s node=%s session=%s taken", owed.TaskID, owed.AttemptID, owed.NodeID, owed.UpstreamID),
		"task", owed.TaskID, "attempt", owed.AttemptID, "node", owed.NodeID, "session", owed.UpstreamID)
}

// owedCloseKey names the native session a close is owed for; at most one
// close is owed per session.
func owedCloseKey(owed state.OwedClose) string {
	return owed.NodeID + "\x00" + owed.HarnessID + "\x00" + owed.UpstreamID
}
