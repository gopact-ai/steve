package attempt

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

func checkRecoveryDeclarationsTx(tx ledger.Reader, desired []project.Project) error {
	all, err := workspaceRecoveriesTx(tx)
	if err != nil {
		return err
	}
	for _, r := range all {
		kept := false
		for _, p := range desired {
			if p.ID == r.Project && project.RecoveryIdentity(p) == r.Declaration {
				kept = true
			}
			for _, w := range p.Workspaces() {
				if w.Node != r.Target.Node {
					continue
				}
				a, b := path.Clean(w.Path), path.Clean(r.Target.Path)
				if a == b && p.ID == r.Project && w.Kind == project.KindCanonical {
					continue
				}
				if a == b || strings.HasPrefix(a, strings.TrimSuffix(b, "/")+"/") || strings.HasPrefix(b, strings.TrimSuffix(a, "/")+"/") {
					return ErrWorkspaceRecovery
				}
			}
		}
		if !kept {
			return fmt.Errorf("%w: project %s still owns its recovery target", ErrWorkspaceRecovery, r.Project)
		}
	}
	return nil
}

func checkRecoveryTaskDeletionTx(tx ledger.Reader, ids []string) error {
	all, err := workspaceRecoveriesTx(tx)
	if err != nil {
		return err
	}
	for _, r := range all {
		for _, source := range r.Sources {
			if slices.Contains(ids, source.Task) {
				return fmt.Errorf("%w: workspace recovery %s", task.ErrRetirementPending, r.ID)
			}
		}
		for _, source := range r.Head.Sources {
			if slices.Contains(ids, source.Execution.TaskID) {
				return fmt.Errorf("%w: recovery output %s", task.ErrRetirementPending, r.ID)
			}
		}
		if r.Producer != nil && slices.Contains(ids, r.Producer.Execution.TaskID) {
			return fmt.Errorf("%w: unpublished recovery output %s", task.ErrRetirementPending, r.ID)
		}
	}
	return nil
}

// RecoveryWorkspaces protects prepared and between-turn directories, even when
// no task or execution is currently live. A read failure is not an empty set.
func (s *Service) RecoveryWorkspaces(ctx context.Context) ([]project.Workspace, error) {
	var out []project.Workspace
	err := s.l.Read(ctx, func(tx *ledger.ReadTx) error {
		all, err := workspaceRecoveriesTx(tx)
		if err != nil {
			return err
		}
		for _, r := range all {
			if r.Workspace.Path != "" {
				out = append(out, r.Workspace)
			}
		}
		return nil
	})
	return out, err
}
