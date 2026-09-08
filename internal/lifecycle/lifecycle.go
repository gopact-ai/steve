// Package lifecycle is what every execution shares once its attempt is
// leased: renewing that lease while the work runs, sending one prompt and
// telling a settled turn from an unconfirmed stop, reading the harness's
// last report as the attempt's usage, closing the session, and the bounded
// context that cleanup runs on after the caller has gone. Chat turns,
// delegations and planning prompts differ in what they prepare and what
// they do with the answer; they do not differ here.
package lifecycle

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/view"
)

// CleanupTimeout bounds what runs after the caller's context is gone:
// recording the outcome, closing the session, giving leases back.
const CleanupTimeout = 15 * time.Second

// Cleanup is the context cleanup runs on: detached from the caller's
// cancellation, because a cancelled turn is still a fact to record, and
// bounded, because nothing should wait on a machine that is not answering.
func Cleanup(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), CleanupTimeout)
}

// Leaser renews an attempt's leases and reports their loss.
type Leaser interface {
	Heartbeat(ctx context.Context, id string) (lost <-chan struct{})
}

// Keep renews the attempt's leases until stop is called, and calls lost —
// once, from its own goroutine — if a lease is lost first. Callers cancel
// their own work in lost: nothing done after a lease is gone could be
// recorded.
func Keep(ctx context.Context, leases Leaser, id string, lost func()) (stop func()) {
	beat, stop := context.WithCancel(ctx)
	gone := leases.Heartbeat(beat, id)
	go func() {
		select {
		case <-gone:
			if beat.Err() == nil {
				lost()
			}
		case <-beat.Done():
		}
	}()
	return stop
}

// Managed reports whether the node owns the session: it keeps the process
// and the input receipts, and the hub only observes it.
func Managed(session harness.Runner) bool {
	return session != nil && IsManaged(session.ID())
}

// IsManaged is Managed for a session id on a record.
func IsManaged(sessionID string) bool { return strings.HasPrefix(sessionID, "ns_") }

// Stopper is a session that can prove its process has exited.
type Stopper interface {
	Stopped() bool
}

// Stopped reports whether the session's process is known to have stopped:
// the evidence a failed close can rely on.
func Stopped(session harness.Runner) bool {
	proof, ok := session.(Stopper)
	return ok && proof.Stopped()
}

// Drive sends one prompt through a session.
type Drive struct {
	Session harness.Runner
	Prompt  string
	Media   []harness.Media
	// Turn routes the harness's permission and user questions through Ask
	// and AskUser, which needs a TurnRunner; a chat turn and a node-owned
	// session want that. Without it the plain Prompt is used.
	Turn    bool
	Ask     permission.AskFunc
	AskUser acphost.AskUserFunc
	Observe func(view.Progress)
}

// Outcome is how a prompt ended.
type Outcome struct {
	Answer   string
	Activity []string
	// Last is the harness's last report; Usage reads the spend from it.
	Last view.Progress
	// PromptSettled is the harness saying the prompt ended, one way or the
	// other; Stopped is the process being known to have exited.
	PromptSettled bool
	Stopped       bool
	Err           error
}

// Settled reports that the turn is over by either kind of evidence.
func (o Outcome) Settled() bool { return o.PromptSettled || o.Stopped }

// Run sends the prompt and waits for it to end.
func (d Drive) Run(ctx context.Context) Outcome {
	var mu sync.Mutex
	var last view.Progress
	observe := func(p view.Progress) {
		mu.Lock()
		last = p
		mu.Unlock()
		if d.Observe != nil {
			d.Observe(p)
		}
	}
	var out Outcome
	if turn, ok := d.Session.(harness.TurnRunner); ok && d.Turn {
		out.Answer, out.Activity, out.Err = turn.PromptTurn(ctx, d.Prompt, d.Media, d.Ask, d.AskUser, observe)
	} else {
		out.Answer, out.Activity, out.Err = d.Session.Prompt(ctx, d.Prompt, observe)
	}
	out.PromptSettled = acphost.PromptSettled(out.Err)
	out.Stopped = Stopped(d.Session)
	mu.Lock()
	out.Last = last
	mu.Unlock()
	return out
}

// Usage is the harness's last report in the ledger's shape.
func Usage(last view.Progress) *attempt.Usage {
	u := last.Usage
	return &attempt.Usage{
		Model: last.Settings.Model, Input: int64(u.InputTokens), Output: int64(u.OutputTokens),
		CachedRead: int64(u.CacheReadTokens), CachedWrite: int64(u.CacheWriteTokens), Context: int64(u.ContextTokens),
		Reported: u.TokensReported(),
	}
}

// Closer closes sessions where they run.
type Closer interface {
	CloseSession(ctx context.Context, at harness.Placement, id string) error
}

// Close ends a session on a cleanup context. A close that fails on a
// process not known to have stopped is an unconfirmed stop: the caller
// must keep the workspace and the slot until someone verifies the exit.
func Close(parent context.Context, sessions Closer, at harness.Placement, session harness.Runner) error {
	ctx, cancel := Cleanup(parent)
	defer cancel()
	if err := sessions.CloseSession(ctx, at, session.ID()); err != nil && !Stopped(session) {
		return errors.Join(harness.ErrStopUnconfirmed, err)
	}
	return nil
}
