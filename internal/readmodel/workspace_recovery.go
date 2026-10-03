package readmodel

import (
	"context"

	"github.com/gopact-ai/steve/internal/attempt"
)

// RecoveryWorkspace remains visible independently of closed source attempts.
// It exposes location and progress, not the execution tokens kept by its owner.
type RecoveryWorkspace struct {
	ID             string `json:"id"`
	Project        string `json:"project"`
	Phase          string `json:"phase"`
	Node           string `json:"node"`
	Path           string `json:"path"`
	Base           string `json:"base"`
	Head           string `json:"head"`
	Version        int64  `json:"version"`
	Error          string `json:"error,omitempty"`
	CleanupPending bool   `json:"cleanup_pending"`
	Result         string `json:"result,omitempty"`
	Outcome        string `json:"outcome,omitempty"`
}

func (l Ledger) recoveryWorkspaces(ctx context.Context) ([]RecoveryWorkspace, error) {
	if l.Book == nil {
		return nil, nil
	}
	rows, err := attempt.New(l.Book).WorkspaceRecoveries(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]RecoveryWorkspace, 0, len(rows))
	for _, r := range rows {
		result, outcome := "", ""
		if r.Result != nil {
			result, outcome = r.Result.Artifact, r.Result.Outcome
		}
		out = append(out, RecoveryWorkspace{ID: r.ID, Project: r.Project, Phase: r.Phase, Node: r.Workspace.Node, Path: r.Workspace.Path, Base: r.Baseline.Artifact, Head: r.Head.Artifact, Version: r.Head.Version, Error: r.Error, CleanupPending: r.Phase == "released" && r.CopyRemovedAt.IsZero(), Result: result, Outcome: outcome})
	}
	return out, nil
}
