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
	c, err = c.forChannel(req.Source.Channel)
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
	if !ok || tracked.Channel != req.Source.ConversationID || tracked.AnchorMessage != req.Source.MessageID || (req.Admission.ExpectedProject != "" && req.Admission.ExpectedProject != tracked.ProjectID) {
		return Result{}, errors.New("recovery stop does not match the original exchange")
	}
	if tracked.Requester != "" && tracked.Requester != req.Actor.ID {
		return Result{}, errors.New("recovery stop requires the original requester")
	}
	if !cancel || tracked.State == task.StateCancelled {
		return c.checkRetainedStop(ctx, tracked)
	}
	result, ids, stopErr := c.setTaskAside(ctx, c.text.T(i18n.CardTasks), tracked, task.StateCancelled, true)
	if stopErr != nil {
		if len(ids) > 0 {
			result.Text = c.text.T(i18n.TaskCancelled, tracked.ID) + "\n\n" + result.Text
		}
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
	tree, err := c.tasks.Tree(tracked.ID)
	if err != nil {
		return Result{}, err
	}
	return c.checkRetainedStopSnapshot(ctx, tree)
}

func (c *Coordinator) checkRetainedStopSnapshot(ctx context.Context, tree []task.Task) (Result, error) {
	tracked := tree[0]
	if tracked.State != task.StatePaused && tracked.State != task.StateCancelled {
		return Result{}, errors.New("the task is no longer waiting for its execution to stop")
	}
	var ids []string
	var tokens []task.ExecutionToken
	for _, current := range tree {
		if current.State != task.StatePaused && current.State != task.StateCancelled && !current.State.Terminal() {
			return Result{}, errors.New("the task is no longer waiting for its execution to stop")
		}
		ids = append(ids, current.ID)
		tokens = append(tokens, task.ExecutionToken{TaskID: current.ID, Epoch: current.ExecutionEpoch})
	}
	stopErr := c.stopExecutionsBefore(ctx, ids, true, tokens)
	if stopErr != nil {
		return Result{}, errors.Join(harness.ErrStopUnconfirmed, stopErr)
	}
	text := c.text.T(i18n.TaskCancelled, tracked.ID)
	if tracked.State == task.StatePaused {
		text = c.text.T(i18n.TaskPaused, tracked.ID, protocol.CommandTasks)
	}
	return Result{Title: c.text.T(i18n.CardTasks), Text: text + "\n\n" + c.taskDetail(tracked)}, nil
}
