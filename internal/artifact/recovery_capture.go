package artifact

import (
	"context"
	"errors"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

// CaptureRecoveryResidual is the frozen episode's exact non-destructive R
// consumer. It never moves the canonical name or accepts a caller's SHA.
func (s *Store) CaptureRecoveryResidual(ctx context.Context, id string, driver ledger.Lease) (attempt.WorkspaceRecovery, error) {
	a := attempt.New(s.ledger)
	r, err := a.WorkspaceRecovery(ctx, id)
	if err != nil {
		return r, err
	}
	if r.Residual != nil {
		return r, nil
	}
	p, found, err := s.projects.Get(ctx, r.Project)
	if err != nil {
		return r, err
	}
	if !found || project.RecoveryIdentity(p) != r.Declaration {
		return r, attempt.ErrWorkspaceRecovery
	}
	if r.Phase == "draining" {
		if err := s.ledger.Update(ctx, func(tx *ledger.Tx) error { _, err := attempt.CheckRecoveryFreezeTx(tx, id, driver); return err }); err != nil {
			return r, err
		}
		if err := s.verifyRecoveryCopy(ctx, p, r); err != nil {
			return r, err
		}
		r, err = a.FreezeRecovery(ctx, id, driver)
		if err != nil {
			return r, err
		}
	}
	err = s.underCanonical(ctx, p, "recovery-residual:"+r.ID, func(ctx context.Context, held ledger.Lease) error {
		if err := s.ledger.Update(ctx, func(tx *ledger.Tx) error {
			_, err := attempt.CheckRecoveryCaptureTx(tx, r.ID, r.FrozenHeadVersion, driver, held)
			return err
		}); err != nil {
			return err
		}
		repo, err := s.Repo(ctx, p.ID)
		if err != nil {
			return err
		}
		var sha string
		var changed bool
		var nested []string
		message := "residual of " + r.ID
		if p.Home.Node == "" {
			sha, changed, nested, err = repo.SnapshotWithNested(ctx, p.Home.Path, r.Baseline.Artifact, message, false)
		} else {
			sha, changed, nested, err = s.snapshotOnNode(ctx, p.Home.Node, p, p.Home.Path, r.Baseline.Artifact, message, repo, false)
		}
		if err != nil {
			return err
		}
		m := Manifest{ID: sha, Project: p.ID, Parent: r.Baseline.Artifact, Label: p.Level, By: r.ID, Message: message, Canonical: true}
		if !changed {
			accepted, found, err := s.Manifest(ctx, sha)
			if err != nil {
				return err
			}
			if found {
				m = accepted
			}
		}
		guard := recordGuard{lease: &held, check: func(tx *ledger.Tx) error {
			_, err := attempt.CheckRecoveryCaptureTx(tx, r.ID, r.FrozenHeadVersion, driver, held)
			return err
		}, accepted: func(tx *ledger.Tx, m Manifest) error {
			content, err := s.RecoveryOutputTx(tx, r.Project, r.Baseline.Artifact, m.ID)
			if err != nil {
				return err
			}
			return attempt.AcceptRecoveryResidualTx(tx, r.ID, r.FrozenHeadVersion, driver, held, attempt.RecoveryResidual{Artifact: m.ID, RecoveryContent: content, CapturedAt: s.now().UTC(), ExcludedNested: nested})
		}}
		_, err = s.receipt(ctx, p, m, guard)
		return err
	})
	if err != nil {
		return r, err
	}
	return a.WorkspaceRecovery(ctx, id)
}

func (s *Store) verifyRecoveryCopy(ctx context.Context, p project.Project, r attempt.WorkspaceRecovery) error {
	if r.Producer != nil || r.Phase != "draining" {
		return attempt.ErrWorkspaceRecovery
	}
	if r.Workspace.Path == "" {
		return nil
	}
	repo, err := s.Repo(ctx, p.ID)
	if err != nil {
		return err
	}
	var changed bool
	if r.Workspace.Node == "" {
		_, changed, _, err = repo.SnapshotWithNested(ctx, r.Workspace.Path, r.Head.Artifact, "verify frozen recovery head", false)
	} else {
		_, changed, _, err = s.snapshotOnNode(ctx, r.Workspace.Node, p, r.Workspace.Path, r.Head.Artifact, "verify frozen recovery head", repo, false)
	}
	if err != nil {
		return err
	}
	if changed {
		return errors.New("recovery copy has unpublished content outside its accepted head")
	}
	return nil
}
