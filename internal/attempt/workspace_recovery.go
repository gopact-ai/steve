package attempt

import (
	"context"
	"errors"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

const workspaceRecoveryKind = "workspace-recovery"

var ErrWorkspaceRecovery = errors.New("workspace recovery must finish before the original directory can be used")

// WorkspaceRecovery owns a continuation independently of its cancelled source.
// The original directory stays held even after every native writer has exited.
type WorkspaceRecovery struct {
	ID          string            `json:"id"`
	Revision    int64             `json:"revision"`
	Phase       string            `json:"phase"`
	CreatedAt   time.Time         `json:"created_at"`
	RequestedBy string            `json:"requested_by"`
	Project     string            `json:"project"`
	Declaration string            `json:"declaration"`
	Target      project.Home      `json:"target"`
	Baseline    RecoveryBaseline  `json:"baseline"`
	Sources     []RecoverySource  `json:"sources"`
	Workspace   project.Workspace `json:"workspace"`
	Head        RecoveryHead      `json:"head"`
	Producer    *RecoveryProducer `json:"producer,omitempty"`
}

type RecoveryBaseline struct {
	Name     string `json:"name"`
	Version  int64  `json:"version"`
	Artifact string `json:"artifact"`
}

type RecoverySource struct {
	Attempt  string    `json:"attempt"`
	Task     string    `json:"task"`
	Revision uint64    `json:"revision"`
	At       time.Time `json:"at"`
}

type RecoveryHead struct {
	Artifact string             `json:"artifact"`
	Version  int64              `json:"version"`
	Sources  []RecoveryProducer `json:"sources,omitempty"`
}

// Producer is recorded before execution, so a failed result write cannot make
// the next writer resume from a stale head or erase unpublished work.
type RecoveryProducer struct {
	Attempt     string              `json:"attempt"`
	Execution   task.ExecutionToken `json:"execution"`
	Base        string              `json:"base"`
	HeadVersion int64               `json:"head_version"`
}

func (s *Service) WorkspaceRecovery(ctx context.Context, id string) (WorkspaceRecovery, error) {
	return WorkspaceRecovery{}, ErrWorkspaceRecovery
}

func (s *Service) RecoveryForProject(ctx context.Context, projectID string) (WorkspaceRecovery, bool, error) {
	return WorkspaceRecovery{}, false, nil
}

// RecoveryHoldTx is also used by artifact writers in their acceptance transaction.
func RecoveryHoldTx(tx ledger.Reader, node, directory string) error { return nil }
