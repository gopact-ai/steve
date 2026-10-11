package turn

import (
	"context"
	"errors"

	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

func (c commands) taskComplete(ctx context.Context, req Request, title string, tracked task.Task) (Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	complete := func() error {
		current, ok := c.tasks.Get(tracked.ID)
		if !ok || current.Channel != tracked.Channel {
			return task.ErrCompleteRoot
		}
		if !current.CompletedByUser && (c.cancels[sessionKey(current.Channel, current.Member)] != nil || c.retiring[current.Channel]) {
			return task.ErrCompleteBusy
		}
		if _, ok := c.plans.ForTask(current.ID); ok {
			return task.ErrCompleteRoot
		}
		guard := func(tx *ledger.Tx, ids map[string]bool) error {
			return c.checkTaskCompletionTx(tx, ids, current.Channel, req.Admission.ExchangeID, false)
		}
		_, err := c.tasks.CompleteRoot(ctx, current.ID, current.Channel, guard)
		return err
	}
	var err error
	if !tracked.CompletedByUser {
		err = c.executions.WhileTaskIdle(tracked.ID, complete)
	} else {
		err = complete()
	}
	if err != nil {
		key := i18n.TaskCompleteFailed
		switch {
		case errors.Is(err, task.ErrCompleteRoot):
			key = i18n.TaskCompleteRoot
		case errors.Is(err, task.ErrCompleteState):
			key = i18n.TaskCompleteState
		case errors.Is(err, task.ErrCompleteBusy):
			key = i18n.TaskCompleteBusy
		case errors.Is(err, task.ErrCompleteChildren):
			key = i18n.TaskCompleteChildren
		case errors.Is(err, task.ErrCompleteDelivery):
			key = i18n.TaskCompleteDelivery
		case errors.Is(err, task.ErrCompleteAttention):
			key = i18n.TaskCompleteAttention
		}
		text := c.text.T(key, tracked.ID)
		return Result{Title: title, Text: text}, UserError{Text: text}
	}
	return Result{Title: title, Text: c.text.T(i18n.TaskCompleted, tracked.ID)}, nil
}
