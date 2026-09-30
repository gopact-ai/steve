package turn

import (
	"context"
	"errors"

	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/task"
)

// StopRetainedTask stops the original recovery task, including its delegated
// children. No active turn or currently selected Agent is needed to identify it.
// An explicit stop cancels the task. A check only retries a stop already
// requested by a pause or cancellation; it never stops resumed work.
func (c *Coordinator) StopRetainedTask(ctx context.Context, taskID string, req Request, cancel bool) (Result, error) {
	ctx = turnContext(ctx)
	c.requestMu.RLock()
	defer c.requestMu.RUnlock()
	var err error
	c, err = c.forChannel(req.Channel)
	if err != nil {
		return Result{}, err
	}
	if req.Locale != "" {
		c = c.localized(i18n.FromLang(req.Locale))
	}
	if c.maintaining {
		return Result{}, errors.New("recovery task control is unavailable")
	}
	tracked, ok := c.tasks.Get(taskID)
	if !ok || tracked.Channel != req.ConversationID || tracked.AnchorMessage != req.MessageID || (req.ExpectedProject != "" && req.ExpectedProject != tracked.ProjectID) {
		return Result{}, errors.New("recovery stop does not match the original exchange")
	}
	if tracked.Requester != "" && tracked.Requester != req.SenderOpenID {
		return Result{}, errors.New("recovery stop requires the original requester")
	}
	if !cancel {
		return c.checkRetainedStop(ctx, tracked)
	}
	result, _, stopErr := c.setTaskAside(ctx, c.text.T(i18n.CardTasks), tracked, task.StateCancelled, true)
	if stopErr != nil {
		return result, errors.Join(harness.ErrStopUnconfirmed, stopErr)
	}
	return result, nil
}

// A check observes the existing revocation instead of revoking the task again.
// If any unfinished descendant was picked back up, it cannot stop that new work.
func (c *Coordinator) checkRetainedStop(ctx context.Context, tracked task.Task) (Result, error) {
	if tracked.State != task.StatePaused && tracked.State != task.StateCancelled {
		return Result{}, errors.New("the task is no longer waiting for its execution to stop")
	}
	pending := []task.Task{tracked}
	var ids []string
	for len(pending) > 0 {
		current := pending[0]
		pending = pending[1:]
		if current.State != task.StatePaused && current.State != task.StateCancelled && !current.State.Terminal() {
			return Result{}, errors.New("the task is no longer waiting for its execution to stop")
		}
		ids = append(ids, current.ID)
		pending = append(pending, c.tasks.Children(current.ID)...)
	}
	stopErr := c.stopExecutions(ctx, ids, true)
	if stopErr != nil {
		return Result{}, errors.Join(harness.ErrStopUnconfirmed, stopErr)
	}
	text := c.text.T(i18n.TaskCancelled, tracked.ID)
	if tracked.State == task.StatePaused {
		text = c.text.T(i18n.TaskPaused, tracked.ID, protocol.CommandTasks)
	}
	return Result{Title: c.text.T(i18n.CardTasks), Text: text + "\n\n" + c.taskDetail(tracked)}, nil
}
