package turn

import (
	"github.com/gopact-ai/steve/internal/schedule"
	"github.com/gopact-ai/steve/internal/task"
)

// SetSchedules enables the scheduling verbs. Without a store they answer that
// scheduling is off rather than pretending to have remembered something.
func (c *Coordinator) SetSchedules(store *schedule.Store) { c.schedules = store }

// RotateTask closes the task an unattended run opened last time, so a
// schedule that fires for weeks gets a fresh budget on each run instead of
// spending one task's allowance a turn at a time. A task the user has since
// taken over — a different origin — is left alone, and so is one still
// running: the new prompt simply queues behind it.
func (c *Coordinator) RotateTask(conversationID, agentID, origin string) {
	if c.tasks == nil || origin == "" {
		return
	}
	c.mu.Lock()
	busy := c.cancels[sessionKey(conversationID, agentID)] != nil
	c.mu.Unlock()
	if busy {
		return
	}
	tracked, ok := c.tasks.Active(conversationID, agentID, origin)
	if !ok {
		return
	}
	if _, err := c.tasks.Advance(tracked.ID, task.StateDone); err != nil {
		return
	}
}
