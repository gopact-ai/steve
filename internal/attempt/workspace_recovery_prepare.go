package attempt

import (
	"context"
	"errors"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

func (s *Service) SelectRecoveryWorkspace(ctx context.Context, id string, ws project.Workspace, by string) (WorkspaceRecovery, error) {
	var result WorkspaceRecovery
	err := s.l.Update(ctx, func(tx *ledger.Tx) error {
		r, err := recoveryByIDTx(tx, id)
		if err != nil {
			return err
		}
		if err := checkRecoveryDeclarationTx(tx, r); err != nil {
			return err
		}
		for _, source := range r.Sources {
			original, err := GetTx(tx, source.Attempt)
			if err != nil {
				return err
			}
			if original.Abandoned == nil || original.Abandoned.WorkspaceRecoveryID != id || original.Abandoned.ProjectedAt.IsZero() {
				return errors.New("recovery awaits original context retirement")
			}
		}
		if ws.RecoveryID != id || ws.Project != r.Project || ws.Kind != project.KindWorktree || ws.Base != r.Baseline.Artifact || ws.ID == "" || ws.Path == "" {
			return ErrWorkspaceRecovery
		}
		if r.Workspace.Path != "" {
			if r.Workspace != ws {
				return errors.New("recovery already has a fixed workspace on another node or location")
			}
			result = r
			return nil
		}
		if r.Phase != "recorded" {
			return ErrWorkspaceRecovery
		}
		r.Workspace, r.Phase = ws, "materializing"
		if err := saveWorkspaceRecoveryTx(tx, &r, by); err != nil {
			return err
		}
		result = r
		return nil
	})
	return result, err
}

func checkRecoveryDeclarationTx(tx ledger.Reader, r WorkspaceRecovery) error {
	p, err := project.ReadTx(tx, r.Project)
	if err != nil {
		return err
	}
	if project.RecoveryIdentity(p) != r.Declaration {
		return ErrWorkspaceRecovery
	}
	return nil
}
