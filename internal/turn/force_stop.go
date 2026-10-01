package turn

import (
	"context"
	"errors"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
)

type ForceStopControl struct{ coordinator *Coordinator }

func NewForceStopControl(c *Coordinator) *ForceStopControl { return &ForceStopControl{coordinator: c} }

// ForceStopAttempt records the owner's cancellation and requests immediate
// native termination. The durable reconciler performs it, not this request.
func (f *ForceStopControl) ForceStopAttempt(ctx context.Context, id, requester string, expectedRevision uint64) error {
	c := f.coordinator
	c.requestMu.RLock()
	defer c.requestMu.RUnlock()
	owner, err := c.owners.of("console")
	if err != nil {
		return err
	}
	if requester == "" || requester != owner {
		return errors.New("force stop requires the owner")
	}
	if c.maintaining {
		return errors.New("force stop is unavailable during maintenance")
	}
	original, err := c.attempts.Get(ctx, id)
	if err != nil {
		return err
	}
	tracked, ok := c.tasks.Get(original.TaskID)
	if !ok {
		return errors.New("original task is unavailable")
	}
	_, err = c.tasks.CancelWith(ctx, tracked.ID, func(tx *ledger.Tx, ids []string) error {
		return c.attempts.RequestForceStopTreeTx(tx, id, tracked.ID, requester, ids)
	})
	if errors.Is(err, attempt.ErrForceStopAlreadyStopped) {
		return nil
	}
	return err
}
