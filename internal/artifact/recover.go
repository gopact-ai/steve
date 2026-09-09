package artifact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/gopact-ai/steve/internal/artifact/ops"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

// LandRecoveryPending is a landing cut off while applying: the WAL says
// which paths were started; the canonical workspace says which landed.
const LandRecoveryPending = "recovery-pending"

// RecoverLandings is run at boot. A landing that never reached applying
// wrote nothing and is closed as interrupted. One cut off mid-apply goes
// recovery-pending, takes the canonical lock again under a new epoch, and
// finishes path by path: a path already at the merged content is skipped,
// one still at the old content is rewritten, anything else is a conflict
// that stops the commit but not the other paths.
func (s *Store) RecoverLandings(ctx context.Context) ([]Landing, error) {
	ops, err := s.ledger.Operations(ctx, landKind, "")
	if err != nil {
		return nil, err
	}
	var out []Landing
	for _, op := range ops {
		var land Landing
		if err := json.Unmarshal(op.Data, &land); err != nil {
			continue
		}
		if _, _, err := s.projects.Get(ctx, land.Project); errors.Is(err, project.ErrNotOwner) {
			continue
		} else if err != nil {
			return out, err
		}
		land.State = op.State
		if land.Recoverable {
			if err := s.ledger.Invalidate(ctx, "landing-driver:"+land.ID); err != nil {
				return out, err
			}
			if land.Lease != nil {
				if err := s.ledger.ReleaseAny(ctx, *land.Lease); err != nil && !errors.Is(err, ledger.ErrStale) {
					return out, err
				}
			}
		}
		switch op.State {
		case LandProposed, LandLocked, LandMerged:
			if land.Recoverable {
				out = append(out, land)
				continue
			}
			land.Lease = nil
			s.failed(ctx, &land, op.State, LandMergeConflicted, "interrupted before apply; nothing was written", nil)
			out = append(out, land)
		case LandApplying, LandRecoveryPending:
			recovered, err := s.recoverLanding(ctx, land)
			if err != nil {
				return out, err
			}
			out = append(out, recovered)
		}
	}
	return out, nil
}

func (s *Store) recoverLanding(ctx context.Context, land Landing) (Landing, error) {
	p, ok, err := s.projects.GetHistorical(ctx, land.Project)
	if err != nil || !ok {
		return land, fmt.Errorf("landing %s: project %s is unknown", land.ID, land.Project)
	}
	if land.Target.Path == "" || land.Target != p.Home {
		return land, fmt.Errorf("landing %s: original target no longer matches project metadata", land.ID)
	}
	if err := s.ledger.Update(ctx, func(tx *ledger.Tx) error { return attempt.CheckWriterTx(tx, p.Home.Node, p.Home.Path) }); err != nil {
		return land, err
	}
	if land.State == LandApplying {
		land.Lease = nil
		if err := s.move(ctx, &land, LandApplying, LandRecoveryPending, nil); err != nil {
			return land, err
		}
	}
	// Nothing may be written before the lock is held again.
	lease, err := s.ledger.AcquireIn(ctx, s.homeRegion(ctx, p), "canonical:"+p.ID, landingHolder(ctx, land.ID), landTTL)
	if err != nil {
		return land, fmt.Errorf("landing %s: %w", land.ID, err)
	}
	land.Lease = &lease
	defer s.releaseCanonical(ctx, &land, lease)
	defer trackLandingLease(ctx, lease)()

	land.Round++
	journal := s.ledger.Journal()
	var conflicted, rewritten []string
	for _, path := range land.Paths {
		state, err := s.pathState(ctx, p, land, path)
		if err != nil {
			return land, err
		}
		switch state {
		case "merged":
			continue
		case "old":
			if _, err := journal.Started(landPathEffect(land, path), "", nil); err != nil {
				return land, err
			}
			if err := s.writeFromTree(ctx, p, land.Merged, path); err != nil {
				return land, err
			}
			if _, err := journal.Confirmed(landPathEffect(land, path), nil); err != nil {
				return land, err
			}
			rewritten = append(rewritten, path)
		default:
			conflicted = append(conflicted, path)
		}
	}
	if len(conflicted) > 0 {
		s.failed(ctx, &land, LandRecoveryPending, LandApplyConflicted, "recovery: paths changed underneath", conflicted)
		return land, nil
	}
	current, _, _ := s.ledger.Name(ctx, CanonicalRef(p.ID))
	land.State = LandCommitted
	land.EndedAt = s.now().UTC()
	_, err = s.ledger.Transition(ctx, land.ID, LandRecoveryPending, LandCommitted, "recovery", landingFence(ctx, []ledger.Lease{lease}),
		map[string]any{"paths": land.Paths, "rewritten": rewritten, "round": land.Round},
		func(tx *ledger.Tx, op *ledger.Operation) error {
			if current.Artifact != land.Merged {
				if _, err := tx.CompareAndSetName(CanonicalRef(p.ID), current.Version, land.Merged); err != nil {
					return err
				}
			}
			return tx.SetData(op, land)
		})
	if err != nil {
		s.failed(ctx, &land, LandRecoveryPending, LandCommitConflict, err.Error(), land.Paths)
		return land, nil
	}
	if _, err := s.receipt(ctx, p, Manifest{ID: land.Merged, Project: p.ID, Parent: land.Now, Label: p.Level, By: land.ID, Message: "landed " + short(land.Artifact) + " (recovered)", Canonical: true}); err != nil {
		return land, err
	}
	return land, nil
}

// pathState compares the canonical file with the merged and the old tree:
// "merged", "old", or "other".
func (s *Store) pathState(ctx context.Context, p project.Project, land Landing, path string) (string, error) {
	bare, err := s.bareFor(ctx, p)
	if err != nil {
		return "", err
	}
	result, err := s.operation(ctx, p.Home.Node, ops.Request{
		Op: ops.PathState, Repo: bare, WorkTree: p.Home.Path,
		From: land.Now, Commit: land.Merged, Path: path,
	})
	if err != nil {
		return "", fmt.Errorf("inspect %s: %w", path, err)
	}
	return result.State, nil
}

// writeFromTree brings one canonical path to its content in tree, deleting
// it when the tree has none.
func (s *Store) writeFromTree(ctx context.Context, p project.Project, tree, path string) error {
	bare, err := s.bareFor(ctx, p)
	if err != nil {
		return err
	}
	_, err = s.operation(ctx, p.Home.Node, ops.Request{
		Op: ops.WritePath, Repo: bare, WorkTree: p.Home.Path, Commit: tree, Path: path,
	})
	return err
}

// bareFor is the shadow repository holding the project's objects where
// its canonical workspace lives.
func (s *Store) bareFor(ctx context.Context, p project.Project) (string, error) {
	if p.Home.Node == "" {
		return filepath.Join(s.Dir, "objects", p.ID+".git"), nil
	}
	_, _, state, err := s.nodes.Git(ctx, p.Home.Node)
	if err != nil {
		return "", err
	}
	return nodeBare(state, p.ID), nil
}
