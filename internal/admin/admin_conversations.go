package admin

import (
	"context"
	"errors"
	"fmt"

	"github.com/gopact-ai/steve/internal/console"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn"
)

// DeleteConversation removes a thread and everything that only existed
// for it: the agent sessions it holds, the tasks it opened with their
// delegations, the schedules that fire into it, and its transcript. It
// is refused while a line of it is still running, because stopping the
// work is the owner's decision, not a side effect of deleting a row.
func (a *Service) DeleteConversation(ctx context.Context, conversation string) error {
	if a.Console == nil {
		return errors.New(textFor(ctx).T(i18n.AdminConversationsOff))
	}
	id := console.ConversationID(conversation)
	release, err := a.Console.Seal(id)
	if err != nil {
		return deleteRefusal(textFor(ctx), err)
	}
	defer release()
	if a.Coordinator != nil {
		if err := a.Coordinator.DiscardConversation(ctx, id); err != nil {
			return deleteRefusal(textFor(ctx), err)
		}
	}
	if err := a.Console.Discard(id); err != nil {
		return deleteRefusal(textFor(ctx), err)
	}
	return nil
}

// deleteRefusal says in the owner's words why a delete did not happen.
func deleteRefusal(text i18n.Catalog, err error) error {
	switch {
	case errors.Is(err, task.ErrExecuting):
		return fmt.Errorf(text.T(i18n.AdminDeleteTaskRunning), err)
	case errors.Is(err, turn.ErrConversationBusy), errors.Is(err, consoleapi.ErrBusy):
		return fmt.Errorf(text.T(i18n.AdminDeleteTurnRunning), err)
	case errors.Is(err, consoleapi.ErrConsoleClosing):
		return fmt.Errorf(text.T(i18n.AdminDeleteHubMaintenance), err)
	default:
		return err
	}
}

// conversationsOf lists the console threads working in one project.
func (a *Service) conversationsOf(ctx context.Context, project string) []string {
	if a.Console == nil {
		return nil
	}
	var out []string
	for _, c := range a.Console.Summaries(ctx) {
		if c.Project == project {
			out = append(out, c.ID)
		}
	}
	return out
}
