package artifact

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/gopact-ai/steve/internal/artifact/ops"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
)

// CleanupRecoveryCopy removes only the fixed platform container after release.
// The declaration claim remains until the exact absence acknowledgement commits.
func (s *Store) CleanupRecoveryCopy(ctx context.Context, id string, driver ledger.Lease) (attempt.WorkspaceRecovery, error) {
	a := attempt.New(s.ledger)
	r, err := a.WorkspaceRecovery(ctx, id)
	if err != nil {
		return r, err
	}
	if !r.CopyRemovedAt.IsZero() {
		return r, nil
	}
	if err := s.ledger.Update(ctx, func(tx *ledger.Tx) error { _, err := attempt.CheckRecoveryCleanupTx(tx, id, driver); return err }); err != nil {
		return r, err
	}
	if r.Workspace.Path != "" {
		root := s.Dir
		if r.Workspace.Node != "" {
			_, root, _, err = s.nodes.Git(ctx, r.Workspace.Node)
			if err != nil {
				return r, err
			}
		}
		request := ops.Request{Op: ops.InspectRecovery, WorkTree: root, Path: filepath.Dir(r.Workspace.Path), Recovery: r.ID, Commit: r.Baseline.Artifact, Identity: r.CopyIdentity, RootIdentity: r.CopyRootIdentity}
		generation := s.generationOf(ctx, r.Workspace.Node)
		if generation < 1 {
			return r, attempt.ErrWorkspaceRecovery
		}
		observed, err := s.operation(ctx, r.Workspace.Node, request)
		if err != nil {
			return r, err
		}
		if r.CopyIdentity != "" && (r.CopyGeneration != generation || r.CopyRootIdentity != observed.RootIdentity) {
			return r, attempt.ErrWorkspaceRecovery
		}
		if observed.Has {
			if r.CopyIdentity == "" {
				if err := s.verifyReleasedRecoveryCopy(ctx, r); err != nil {
					return r, err
				}
			} else if err := s.verifyPartialRecoveryCopy(ctx, r); err != nil {
				return r, err
			}
			if err := a.RecordRecoveryContainer(ctx, id, driver, observed.Identity, observed.RootIdentity, generation); err != nil {
				return r, err
			}
			request.Op, request.Identity, request.RootIdentity = ops.RemoveRecovery, observed.Identity, observed.RootIdentity
			if err := s.ledger.Update(ctx, func(tx *ledger.Tx) error { _, err := attempt.CheckRecoveryCleanupTx(tx, id, driver); return err }); err != nil {
				return r, err
			}
			deleted, err := s.operation(ctx, r.Workspace.Node, request)
			if err != nil {
				return r, err
			}
			if s.generationOf(ctx, r.Workspace.Node) != generation || deleted.RootIdentity != observed.RootIdentity {
				return r, attempt.ErrWorkspaceRecovery
			}
			if deleted.Has {
				return r, attempt.ErrWorkspaceRecovery
			}
		}
	}
	if err := a.ConfirmRecoveryCopyRemoved(ctx, id, driver); err != nil {
		return r, err
	}
	return a.WorkspaceRecovery(ctx, id)
}

func (s *Store) verifyReleasedRecoveryCopy(ctx context.Context, r attempt.WorkspaceRecovery) error {
	p, found, err := s.projects.Get(ctx, r.Project)
	if err != nil {
		return err
	}
	if !found {
		return attempt.ErrWorkspaceRecovery
	}
	repo, err := s.Repo(ctx, p.ID)
	if err != nil {
		return err
	}
	var sha string
	var changed bool
	var nested []string
	if r.Workspace.Node == "" {
		sha, changed, nested, err = repo.SnapshotWithNested(ctx, r.Workspace.Path, r.Head.Artifact, "verify released recovery copy", false)
	} else {
		sha, changed, nested, err = s.snapshotOnNode(ctx, r.Workspace.Node, p, r.Workspace.Path, r.Head.Artifact, "verify released recovery copy", repo, false)
	}
	if err != nil {
		return err
	}
	if changed || sha != r.Head.Artifact || len(nested) > 0 {
		return errors.New("recovery copy contains unpublished or excluded content; cleanup remains pending")
	}
	request := ops.Request{Op: ops.VerifyRecoveryContent, WorkTree: r.Workspace.Path, Repo: repo.Dir, Commit: r.Head.Artifact}
	if r.Workspace.Node != "" {
		_, _, state, err := s.nodes.Git(ctx, r.Workspace.Node)
		if err != nil {
			return err
		}
		request.Repo = nodeBare(state, p.ID)
	}
	_, err = s.operation(ctx, r.Workspace.Node, request)
	return err
}

func (s *Store) verifyPartialRecoveryCopy(ctx context.Context, r attempt.WorkspaceRecovery) error {
	repo, err := s.Repo(ctx, r.Project)
	if err != nil {
		return err
	}
	request := ops.Request{Op: ops.VerifyRecoveryRemainder, Repo: repo.Dir, Commit: r.Head.Artifact, WorkTree: r.Workspace.Path}
	if r.Workspace.Node != "" {
		_, _, state, err := s.nodes.Git(ctx, r.Workspace.Node)
		if err != nil {
			return err
		}
		request.Repo = nodeBare(state, r.Project)
	}
	_, err = s.operation(ctx, r.Workspace.Node, request)
	return err
}
