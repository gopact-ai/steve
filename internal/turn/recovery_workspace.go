package turn

import (
	"context"
	"errors"
	"fmt"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

func (c *Coordinator) sharedRecoveryWorkspace(ctx context.Context, req Request, selected agent.Agent, projectID string) (project.Workspace, bool, error) {
	r, found, err := c.attempts.RecoveryForProject(ctx, projectID)
	if err != nil || !found {
		return project.Workspace{}, found, err
	}
	p, exists, err := c.projects.Get(ctx, projectID)
	if err != nil || !exists {
		return project.Workspace{}, true, errors.Join(project.ErrUnknown, err)
	}
	if copy, ok := p.CopyOn(selected.Node); ok && copy.State == project.CopyReady {
		return project.Workspace{}, false, nil
	}
	if project.RecoveryIdentity(p) != r.Declaration {
		return project.Workspace{}, true, attempt.ErrWorkspaceRecovery
	}
	if r.Workspace.Path != "" && r.Workspace.Node != selected.Node {
		return project.Workspace{}, true, fmt.Errorf("project recovery continues on node %s; select an agent there", placeLabel(r.Workspace.Node))
	}
	planned, err := c.artifacts.PlanRecoveryWorkspace(ctx, r, selected.Node)
	if err != nil {
		return project.Workspace{}, true, err
	}
	r, err = c.attempts.SelectRecoveryWorkspace(ctx, r.ID, planned, req.SenderOpenID)
	if err != nil {
		return project.Workspace{}, true, err
	}
	r, err = c.attempts.PrepareWorkspaceRecovery(ctx, r, c.artifacts.PrepareRecoveryWorkspace)
	if err != nil {
		return project.Workspace{}, true, err
	}
	r, err = c.attempts.PublishRecoveryHead(ctx, r.ID, validateRecoveryArtifact)
	if err != nil {
		return project.Workspace{}, true, err
	}
	workspace := r.Workspace
	workspace.Base = r.Head.Artifact
	return workspace, true, nil
}

func validateRecoveryArtifact(tx ledger.Reader, projectID, parent, artifactID string) error {
	p, err := project.ReadTx(tx, projectID)
	if err != nil {
		return err
	}
	return artifact.CheckRecoveryOutputTx(tx, p, parent, artifactID)
}
