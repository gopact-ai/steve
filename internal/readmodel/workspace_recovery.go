package readmodel

import (
	"context"

	"github.com/gopact-ai/steve/internal/attempt"
)

// RecoveryWorkspace remains visible independently of closed source attempts.
// It exposes location and progress, not the execution tokens kept by its owner.
type RecoveryWorkspace struct {
	ID      string `json:"id"`
	Project string `json:"project"`
	Phase   string `json:"phase"`
	Node    string `json:"node"`
	Path    string `json:"path"`
	Base    string `json:"base"`
	Head    string `json:"head"`
	Version int64  `json:"version"`
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
		out = append(out, RecoveryWorkspace{ID: r.ID, Project: r.Project, Phase: r.Phase, Node: r.Workspace.Node, Path: r.Workspace.Path, Base: r.Baseline.Artifact, Head: r.Head.Artifact, Version: r.Head.Version})
	}
	return out, nil
}
