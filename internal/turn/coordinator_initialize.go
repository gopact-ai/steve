package turn

import (
	"context"
	"errors"
	"strings"

	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/project"
)

// InitializeConversation sets only the initial project binding. It does not
// switch an existing conversation or change its sessions and tasks.
func (c *Coordinator) InitializeConversation(ctx context.Context, conversation, projectID, requester string) error {
	if strings.TrimSpace(conversation) == "" || strings.TrimSpace(projectID) == "" || strings.TrimSpace(requester) == "" {
		return errors.New("conversation, project and requester are required")
	}
	if _, ok, err := c.projects.Get(ctx, projectID); err != nil {
		return err
	} else if !ok {
		return UserError{Text: c.text.T(i18n.ProjectUnknown, projectID)}
	}
	if err := c.require(ctx, projectID, requester, project.RoleRead); err != nil {
		return err
	}
	_, err := c.projects.BindInitial(ctx, conversation, projectID, requester)
	return err
}
