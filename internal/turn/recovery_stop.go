package turn

import (
	"context"
	"errors"

	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/task"
)

// StopRetainedTask stops the original recovery task, including its delegated
// children. No active turn or currently selected Agent is needed to identify it.
// A task the owner paused stays paused, to be picked back up later; any
// other is cancelled.
func (c *Coordinator) StopRetainedTask(ctx context.Context, taskID string, req Request) (Result, error) {
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
	to := task.StateCancelled
	if tracked.State == task.StatePaused {
		to = task.StatePaused
	}
	result, _, stopErr := c.setTaskAside(ctx, c.text.T(i18n.CardTasks), tracked, to, true)
	if stopErr != nil {
		return result, errors.Join(harness.ErrStopUnconfirmed, stopErr)
	}
	return result, nil
}
