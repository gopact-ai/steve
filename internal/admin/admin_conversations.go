package admin

import (
	"context"
	"errors"
	"fmt"

	"github.com/gopact-ai/steve/internal/console"
	"github.com/gopact-ai/steve/internal/consoleapi"
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
		return errors.New("会话服务没有启用")
	}
	id := console.ConversationID(conversation)
	release, err := a.Console.Seal(id)
	if err != nil {
		return deleteRefusal(err)
	}
	defer release()
	if a.Coordinator != nil {
		if err := a.Coordinator.DiscardConversation(ctx, id); err != nil {
			return deleteRefusal(err)
		}
	}
	if err := a.Console.Discard(id); err != nil {
		return deleteRefusal(err)
	}
	return nil
}

// deleteRefusal says in the owner's words why a delete did not happen.
func deleteRefusal(err error) error {
	switch {
	case errors.Is(err, task.ErrExecuting):
		return fmt.Errorf("会话里还有任务在执行，先停止再删除（%w）", err)
	case errors.Is(err, turn.ErrConversationBusy), errors.Is(err, consoleapi.ErrBusy):
		return fmt.Errorf("会话还有没跑完的回合，先停止再删除（%w）", err)
	case errors.Is(err, consoleapi.ErrConsoleClosing):
		return fmt.Errorf("Hub 正在维护，稍后再删除（%w）", err)
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
