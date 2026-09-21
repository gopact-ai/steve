package app

import (
	"context"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn"
)

// channelConversations combines durable transcript evidence with the same
// read-only project/context and live task owners used by the workbench.
// The channel's opaque identity is never passed to Console normalization.
type channelConversations struct {
	consoleapi.ChannelHistory
	contexts interface {
		Context(context.Context, string) (turn.Context, error)
	}
	tasks interface {
		Query(task.Query) (task.Page, error)
	}
	activity interface {
		ConversationBusy(string) bool
	}
}

func (h *channelConversations) List(ctx context.Context, cursor string, limit int) (consoleapi.ChannelConversationPage, error) {
	page, err := h.ChannelHistory.List(ctx, cursor, limit)
	if err != nil {
		return page, err
	}
	for i := range page.Conversations {
		if err := h.enrich(ctx, &page.Conversations[i]); err != nil {
			return consoleapi.ChannelConversationPage{}, err
		}
	}
	return page, nil
}

func (h *channelConversations) Read(ctx context.Context, id, cursor string, limit int) (consoleapi.ChannelConversationHistory, error) {
	page, err := h.ChannelHistory.Read(ctx, id, cursor, limit)
	if err != nil {
		return page, err
	}
	if err := h.enrich(ctx, &page.Conversation); err != nil {
		return consoleapi.ChannelConversationHistory{}, err
	}
	return page, nil
}

func (h *channelConversations) enrich(ctx context.Context, item *consoleapi.Conversation) error {
	if h.contexts != nil {
		standing, err := h.contexts.Context(ctx, item.ID)
		if err != nil {
			return err
		}
		if standing.Project != nil && standing.Project.Bound {
			item.Project = standing.Project.ID
		}
		if standing.Agent != nil {
			item.Agent = standing.Agent.ID
			if p := standing.Agent.Place; p != nil {
				item.Place = &consoleapi.Placement{Workspace: p.Workspace, Kind: p.Kind, Node: p.Node}
			}
		}
	}
	item.Running = h.activity != nil && h.activity.ConversationBusy(item.ID)
	item.Execution = "idle"
	if item.Running {
		item.Execution = "running"
	}
	if h.tasks == nil {
		return nil
	}
	q := task.Query{Scope: task.Scope{Kind: "conversation", ID: item.ID}, Status: "live", Limit: task.MaxQueryLimit}
	for {
		page, err := h.tasks.Query(q)
		if err != nil {
			return err
		}
		for _, t := range page.Items {
			if t.Transport != item.Transport {
				continue
			}
			if item.Project == "" {
				item.Project = t.ProjectID
			}
			if t.Summary.OpenExecutions > 0 && !item.Running {
				item.Execution = "unknown"
			}
		}
		if page.NextCursor == "" {
			return nil
		}
		q.Cursor = page.NextCursor
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}
