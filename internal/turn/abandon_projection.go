package turn

import (
	"context"
	"errors"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/state"
)

// ProjectAbandoned replays retirement after the irreversible decision. A
// transient failure leaves the original path and endpoint admission fenced.
func (a *AbandonControl) ProjectAbandoned(ctx context.Context, id string) error {
	c := a.coordinator
	c.requestMu.RLock()
	defer c.requestMu.RUnlock()
	r, err := c.attempts.Get(ctx, id)
	if err != nil {
		return err
	}
	if r.Abandoned == nil {
		return errors.New("execution was not abandoned")
	}
	if !r.Abandoned.ProjectedAt.IsZero() {
		return nil
	}
	owed := state.OwedClose{NodeID: r.Node, HarnessID: r.Harness, UpstreamID: r.Abandoned.Session, NativeContext: r.NativeContext, TaskID: r.TaskID, AttemptID: r.ID, OwedAt: r.Abandoned.At.Format(time.RFC3339Nano)}
	guard := func(tx *ledger.Tx) error { return c.attempts.CheckAbandonProjectionTx(tx, r) }
	if r.Abandoned.Session == "" {
		err = c.store.ProjectAbandonedOpen(ctx, r.Abandoned.Conversation, r.Agent, r.Node, r.Harness, r.ID, r.Abandoned.SlotState, r.Abandoned.SlotFingerprint, r.Abandoned.ImportFingerprint, guard)
	} else {
		err = c.store.ProjectAbandonedSession(ctx, r.Abandoned.Conversation, r.Agent, owed, r.Unsettled, guard)
	}
	if err != nil {
		return err
	}
	_, err = c.attempts.ProjectAbandonedCapacity(ctx, id, r.Abandoned.ForceStopRevision)
	return err
}

// PendingAbandonments is replayed by the coordinator independently of physical
// stop candidates, including a process that exits before its archive is written.
func (a *AbandonControl) PendingAbandonments(ctx context.Context) ([]attempt.Record, error) {
	return a.coordinator.attempts.AbandonProjections(ctx)
}

func (a *AbandonControl) ReadAbandoned(ctx context.Context, id string) (attempt.Record, error) {
	return a.coordinator.attempts.Get(ctx, id)
}
