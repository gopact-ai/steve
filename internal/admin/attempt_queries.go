package admin

import (
	"context"
	"errors"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/consoleapi"
)

func (a *Service) QueryAttempts(ctx context.Context, q consoleapi.AttemptHistoryQuery) (consoleapi.AttemptHistoryPage, error) {
	if a.Attempts == nil {
		return consoleapi.AttemptHistoryPage{}, errors.New("attempts are not wired")
	}
	page, err := a.Attempts.QueryHistory(ctx, attempt.HistoryQuery{TaskID: q.TaskID, Conversation: q.Conversation, Cursor: q.Cursor, Limit: q.Limit})
	if err != nil {
		return consoleapi.AttemptHistoryPage{}, err
	}
	out := consoleapi.AttemptHistoryPage{Items: make([]consoleapi.AttemptHistoryItem, 0, len(page.Items)), NextCursor: page.NextCursor}
	for _, r := range page.Items {
		item := consoleapi.AttemptHistoryItem{TaskID: r.TaskID, Project: r.Project, AttemptView: consoleapi.AttemptView{
			ID: r.ID, Kind: string(r.Kind), State: string(r.State), Agent: r.Agent, Node: r.Node, Harness: r.Harness,
			Workspace: r.Workspace.Path, Base: r.Base, Error: r.Error, StartedAt: r.StartedAt, EndedAt: r.EndedAt,
		}}
		if r.Result != nil {
			item.Artifact, item.Summary = r.Result.Artifact, r.Result.Summary
		}
		if item.Artifact == "" || item.Artifact == item.Base {
			item.FilesKnown = true
			if r.Result != nil && r.Result.CaptureError != "" {
				item.FilesKnown = false
				item.FilesError = r.Result.CaptureError
			}
		} else if a.Artifacts != nil {
			changes, truncated, err := a.Artifacts.Changes(ctx, r.Project, item.Base, item.Artifact)
			if err == nil {
				item.Files, item.FilesKnown, item.FilesTruncated = len(changes), true, truncated
			} else {
				item.FilesError = err.Error()
			}
		}
		out.Items = append(out.Items, item)
	}
	return out, nil
}
