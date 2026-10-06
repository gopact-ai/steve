package turn

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/gopact-ai/steve/internal/task"
)

// RotateTask closes the task an unattended run opened last time, so a
// schedule that fires for weeks gets a fresh budget on each run instead of
// spending one task's allowance a turn at a time. A task the user has since
// taken over — a different origin — is left alone, and so is one still
// running: the new prompt simply queues behind it. So is one whose last run
// has not settled (still waiting on the owner, say): the new prompt goes to
// it like any other input, queued behind that run where the channel queues,
// refused with the task's other messages while its execution is open.
func (c *Coordinator) RotateTask(conversationID, agentID, origin string) {
	if origin == "" {
		return
	}
	c.requestMu.RLock()
	defer c.requestMu.RUnlock()
	if c.maintaining {
		return
	}
	release, err := c.beginSessionRetirement(context.Background(), conversationID, agentID)
	if err != nil {
		return
	}
	defer release()
	tracked, ok := c.tasks.Active(conversationID, agentID, origin)
	if !ok {
		return
	}
	_, err = c.closeSettled(context.Background(), []string{tracked.ID}, "")
	switch {
	case task.CompletionRefused(err):
		slog.Info(fmt.Sprintf("turn: scheduled task %s stays open: %v", tracked.ID, err), "task", tracked.ID, "conversation", conversationID, "origin", origin)
	case err != nil:
		slog.Warn(fmt.Sprintf("turn: rotate scheduled task %s: %v", tracked.ID, err), "task", tracked.ID, "conversation", conversationID, "origin", origin)
	}
}
