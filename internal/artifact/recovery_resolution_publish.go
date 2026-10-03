package artifact

import (
	"context"
	"errors"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

// PublishRecoveryResolution keeps ordinary inputs and nested repositories in
// the resolver's exact snapshot scope; ordinary Publish retains its contract.
func (s *Store) PublishRecoveryResolution(ctx context.Context, ref attempt.RecoveryResolutionRef, ws project.Workspace, parent string, source Source) (Manifest, bool, error) {
	p, found, err := s.projects.Get(ctx, ws.Project)
	if err != nil {
		return Manifest{}, false, err
	}
	if !found {
		return Manifest{}, false, project.ErrUnknown
	}
	guard := recordGuard{check: func(tx *ledger.Tx) error {
		current, err := attempt.RecoveryResolutionPublicationTx(tx, ref)
		if err != nil {
			return err
		}
		if current.Project != p.ID || project.RecoveryIdentity(p) != current.Declaration || parent != ref.Marked || ws.RecoveryID != "" || ws.Kind != project.KindWorktree {
			return attempt.ErrWorkspaceRecovery
		}
		if _, err := recoveryConflictTx(tx, current, ref); err != nil {
			return err
		}
		if source.Execution == nil || source.AttemptID == "" {
			return task.ErrExecutionStopped
		}
		r, err := attempt.GetTx(tx, source.AttemptID)
		if err != nil {
			return err
		}
		if r.Project != ws.Project || r.Workspace != ws || r.Execution == nil || *r.Execution != *source.Execution || r.State.Terminal() || r.Unsettled || r.Base != ref.Marked {
			return errors.New("recovery resolver publication differs from its admitted execution")
		}
		if err := task.CheckExecutionTx(tx, r.Execution); err != nil {
			return err
		}
		for _, lease := range r.Leases {
			if lease.Region == "" || lease.Region == s.ledger.Region() {
				if err := tx.CheckLocalLease(lease); err != nil {
					return err
				}
			}
		}
		if err := acceptedWorkspaceArtifactTx(tx, p, parent); err != nil {
			return err
		}
		return acceptedWorkspaceArtifactTx(tx, p, ref.Marked)
	}}
	if err := s.admits(ctx, p, ws.Node); err != nil {
		return Manifest{}, false, err
	}
	record, err := attempt.New(s.ledger).Get(ctx, source.AttemptID)
	if err != nil {
		return Manifest{}, false, err
	}
	for _, lease := range record.Leases {
		if lease.Region != "" && lease.Region != s.ledger.Region() {
			if err := s.ledger.CheckAny(ctx, lease); err != nil {
				return Manifest{}, false, err
			}
		}
	}
	if err := s.ledger.Update(ctx, guard.check); err != nil {
		return Manifest{}, false, err
	}
	return s.snapshotRecoveryResolution(ctx, p, ws, parent, source.AttemptID, guard)
}

func (s *Store) snapshotRecoveryResolution(ctx context.Context, p project.Project, ws project.Workspace, parent, by string, guards ...recordGuard) (Manifest, bool, error) {
	repo, err := s.Repo(ctx, p.ID)
	if err != nil {
		return Manifest{}, false, err
	}
	var sha string
	var changed bool
	if ws.Node == "" {
		sha, changed, _, err = repo.SnapshotWithNested(ctx, ws.Path, parent, "resolved recovery conflict", false)
	} else {
		sha, changed, _, err = s.snapshotOnNode(ctx, ws.Node, p, ws.Path, parent, "resolved recovery conflict", repo, false)
	}
	if err != nil {
		return Manifest{}, false, err
	}
	m := Manifest{ID: sha, Project: p.ID, Parent: parent, Label: p.Level, By: by, Message: "resolved recovery conflict"}
	if !changed {
		accepted, found, err := s.Manifest(ctx, sha)
		if err != nil {
			return m, false, err
		}
		if found {
			m = accepted
		}
	}
	m, err = s.receipt(ctx, p, m, guards...)
	return m, changed, err
}
