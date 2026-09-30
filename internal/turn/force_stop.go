package turn

import (
	"context"
	"errors"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/task"
)

type ForceStopControl struct{ coordinator *Coordinator }

func NewForceStopControl(c *Coordinator) *ForceStopControl { return &ForceStopControl{coordinator: c} }

// ForceStopAttempt records the owner's cancellation and requests immediate
// native termination. The durable reconciler performs it, not this request.
func (f *ForceStopControl) ForceStopAttempt(ctx context.Context, id, requester string) error {
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
	if !attempt.TaskStopOwed(original) || attempt.TaskStopConfirmed(original) {
		return errors.New("execution does not owe a native stop")
	}
	tracked, ok := c.tasks.Get(original.TaskID)
	if !ok {
		return errors.New("original task is unavailable")
	}
	if tracked.State != task.StateCancelled {
		if _, err := c.tasks.SetAside(tracked.ID, task.StateCancelled); err != nil {
			return err
		}
	}
	tree, err := c.tasks.Tree(tracked.ID)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(tree))
	for _, t := range tree {
		ids = append(ids, t.ID)
	}
	records, err := c.attempts.ForTasksByUpdate(ctx, ids)
	if err != nil {
		return err
	}
	var result error
	for _, r := range records {
		if attempt.TaskStopOwed(r) && !attempt.TaskStopConfirmed(r) {
			_, err := c.attempts.RequestForceStop(ctx, r.ID, requester)
			result = errors.Join(result, err)
		}
	}
	return result
}
