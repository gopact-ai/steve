package turn

import (
	"context"
	"fmt"

	"github.com/gopact-ai/steve/internal/project"
)

func checkScheduledProject(expected, actual string) error {
	if expected != "" && expected != actual {
		return fmt.Errorf("scheduled project is %s, but this conversation is now bound to %s; the scheduled work was not run", expected, actual)
	}
	return nil
}

// ValidateScheduled checks the immutable scheduled target without rebinding
// the user's conversation. Execution repeats the project check after queueing.
func (c *Coordinator) ValidateScheduled(ctx context.Context, conversation, expected, requester string) error {
	if expected == "" || requester == "" || c.projects == nil {
		return fmt.Errorf("scheduled project and requester must be recorded")
	}
	binding, ok, err := c.projects.Binding(ctx, conversation)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("scheduled conversation %s no longer has a project binding", conversation)
	}
	if err := checkScheduledProject(expected, binding.ProjectID); err != nil {
		return err
	}
	return c.require(ctx, expected, requester, project.RoleWrite)
}
