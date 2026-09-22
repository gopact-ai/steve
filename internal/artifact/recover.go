package artifact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
// one still at the old content is rewritten, and anything else is a
// conflict that ends the landing before any path is written.
//
// The previous process is gone, so the canonical lock a landing recorded is
// released first. A landing that still cannot be recovered — the lock is
// someone else's, the home node is unreachable — is logged and left
// recovery-pending for RetryRecoveries; it is not a reason to refuse to
// start. Only failing to list the landings at all is returned.
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
		land.State = op.State
		if _, _, err := s.projects.Get(ctx, land.Project); errors.Is(err, project.ErrNotOwner) {
			continue
		} else if err != nil {
			logRecoveryLeft(land, err)
			continue
		}
		if err := s.releaseDeadLanding(ctx, land); err != nil {
			logRecoveryLeft(land, err)
			continue
		}
		switch op.State {
		case LandProposed, LandLocked, LandMerged:
			if land.Recoverable {
				out = append(out, land)
				continue
			}
			land.Lease = nil
			land.Unapplied = true
			s.failed(ctx, &land, op.State, LandMergeConflicted, "interrupted before apply; nothing was written", nil)
			out = append(out, land)
		case LandApplying, LandRecoveryPending:
			recovered, err := s.recoverLanding(ctx, land)
			if err != nil {
				logRecoveryLeft(recovered, err)
				continue
			}
			out = append(out, recovered)
		}
	}
	return out, nil
}

// releaseDeadLanding lets go of what the previous process held for a
// landing it did not finish: the driver of a durable landing, and the
// canonical lock of any landing still in flight. Only the exact lease the
// landing recorded is released; one taken since is someone else's.
func (s *Store) releaseDeadLanding(ctx context.Context, land Landing) error {
	inFlight := false
	switch land.State {
	case LandProposed, LandLocked, LandMerged, LandApplying, LandRecoveryPending:
		inFlight = true
	}
	if land.Recoverable {
		if err := s.ledger.Invalidate(ctx, "landing-driver:"+land.ID); err != nil {
			return err
		}
	}
	if land.Lease != nil && (inFlight || land.Recoverable) {
		if err := s.ledger.ReleaseAny(ctx, *land.Lease); err != nil && !errors.Is(err, ledger.ErrStale) {
			return fmt.Errorf("release canonical lock of the previous process: %w", err)
		}
	}
	return nil
}

// logRecoveryLeft records a landing whose recovery has to wait, with the
// keys an operator filters on.
func logRecoveryLeft(land Landing, err error) {
	slog.Warn(fmt.Sprintf("artifact: landing %s into %s left %s for a later retry: %v", land.ID, land.Project, land.State, err),
		"landing", land.ID, "project", land.Project, "artifact", land.Artifact, "state", land.State, "error", err.Error())
}

// RetryRecoveries finishes the landings that are still recovery-pending:
// ones boot recovery could not finish, or whose recovery was cut off in
// turn. It is run periodically. A landing whose canonical lock is busy, or
// whose own driver is running, is skipped quietly until the next pass; any
// other failure is logged and retried next pass. What was recovered — to
// committed or to a conflict — is returned.
func (s *Store) RetryRecoveries(ctx context.Context) ([]Landing, error) {
	pending, err := s.ledger.Operations(ctx, landKind, LandRecoveryPending)
	if err != nil {
		return nil, err
	}
	var out []Landing
	for _, op := range pending {
		var land Landing
		if err := json.Unmarshal(op.Data, &land); err != nil {
			continue
		}
		land.State = op.State
		if _, _, err := s.projects.Get(ctx, land.Project); errors.Is(err, project.ErrNotOwner) {
			continue
		} else if err != nil {
			logRecoveryLeft(land, err)
			continue
		}
		recovered, err := s.retryRecovery(ctx, land)
		if errors.Is(err, ledger.ErrHeld) {
			continue
		}
		if err != nil {
			logRecoveryLeft(recovered, err)
			continue
		}
		if recovered.State == LandRecoveryPending {
			continue
		}
		out = append(out, recovered)
	}
	return out, nil
}

// retryRecovery recovers one landing. A durable landing is recovered under
// its own driver, the one its caller takes to resume it, so the two never
// run it at the same time.
func (s *Store) retryRecovery(ctx context.Context, land Landing) (Landing, error) {
	if !land.Recoverable {
		return s.recoverLanding(ctx, land)
	}
	ttl := s.landingDriverTTL
	if ttl <= 0 {
		ttl = landTTL
	}
	drive, err := s.ledger.Acquire(ctx, "landing-driver:"+land.ID, attempt.NewID(), ttl)
	if err != nil {
		return land, err
	}
	ctx, stop := s.startLandingDriver(ctx, drive, ttl)
	defer stop()
	apply, cancel := landingApplyContext(ctx)
	defer cancel()
	return s.recoverLanding(apply, land)
}

func (s *Store) recoverLanding(ctx context.Context, land Landing) (Landing, error) {
	if land.State == LandApplying {
		land.Lease = nil
		if err := s.move(ctx, &land, LandApplying, LandRecoveryPending, nil); err != nil {
			return land, err
		}
	}
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
	// Nothing may be written before the lock is held again.
	lease, err := s.ledger.AcquireIn(ctx, s.homeRegion(ctx, p), "canonical:"+p.ID, landingHolder(ctx, land.ID), landTTL)
	if err != nil {
		return land, fmt.Errorf("landing %s: %w", land.ID, err)
	}
	land.Lease = &lease
	defer s.releaseCanonical(ctx, &land, lease)
	defer trackLandingLease(ctx, lease)()
	// Another recovery of the same landing may have finished it while this
	// one waited for the lock; its outcome stands.
	if op, found, err := s.ledger.Operation(ctx, land.ID); err != nil {
		return land, err
	} else if !found || op.State != LandRecoveryPending {
		var stored Landing
		if found {
			if err := json.Unmarshal(op.Data, &stored); err != nil {
				return land, err
			}
			stored.State = op.State
		}
		return stored, nil
	}
	// Recovery reads the merged snapshot where the canonical workspace
	// lives. A landing cut off before it got there still has it on the hub,
	// where it was merged.
	if p.Home.Node != "" && !metadataOnly(p) {
		hub, err := s.Repo(ctx, p.ID)
		if err != nil {
			return land, err
		}
		if err := s.stageMerged(ctx, p, &land, hub); err != nil {
			return land, err
		}
	}

	// What is on disk is snapshotted, under the lock, the way a landing
	// does before merging: the snapshot is what says which nested
	// repositories the canonical workspace has, and it is the canonical a
	// conflict is blocked on — the interrupted round's own writes included,
	// so those turning up in a later snapshot do not start it again.
	_, _, nested, err := s.snapshotCanonical(ctx, p, s.canonicalRef(ctx, p.ID), land.ID, "before recovering "+short(land.Artifact))
	if err != nil {
		return land, fmt.Errorf("landing %s: snapshot before recovery: %w", land.ID, err)
	}
	// A path inside a nested repository was never in any snapshot, so it
	// reads as "old" and would be written into the user's repository. It
	// is refused here as a new landing refuses it.
	if inside, repos := pathsInsideNested(land.Paths, nested); len(inside) > 0 {
		s.conflictedRecovery(ctx, p, &land, inside, nestedReason(repos))
		return land, nil
	}

	// Every path is inspected before any is written: a recovery that ends
	// in a conflict leaves the workspace as it found it.
	var conflicted, stale []string
	for _, path := range land.Paths {
		state, err := s.pathState(ctx, p, land, path)
		if err != nil {
			return land, err
		}
		switch state {
		case "merged":
		case "old":
			stale = append(stale, path)
		default:
			conflicted = append(conflicted, path)
		}
	}
	if len(conflicted) > 0 {
		s.conflictedRecovery(ctx, p, &land, conflicted, "interrupted while applying, and these paths were changed by someone else before recovery could finish them")
		return land, nil
	}
	land.Round++
	journal := s.ledger.Journal()
	for _, path := range stale {
		if _, err := journal.Started(landPathEffect(land, path), "", nil); err != nil {
			return land, err
		}
		if err := s.writeFromTree(ctx, p, land.Merged, path); err != nil {
			return land, err
		}
		if _, err := journal.Confirmed(landPathEffect(land, path), nil); err != nil {
			return land, err
		}
	}
	current, _, _ := s.ledger.Name(ctx, CanonicalRef(p.ID))
	land.State = LandCommitted
	land.EndedAt = s.now().UTC()
	_, err = s.ledger.Transition(ctx, land.ID, LandRecoveryPending, LandCommitted, "recovery", landingFence(ctx, []ledger.Lease{lease}),
		map[string]any{"paths": land.Paths, "rewritten": stale, "round": land.Round},
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

// conflictedRecovery ends a recovery that wrote nothing as apply-conflicted
// on paths, and keeps a queued result blocked on it the way a landing's own
// conflict is kept, with the reason a person can act on.
func (s *Store) conflictedRecovery(ctx context.Context, p project.Project, land *Landing, paths []string, reason string) {
	s.failed(ctx, land, LandRecoveryPending, LandApplyConflicted, "recovery: "+reason, paths)
	if !land.Recoverable {
		s.queueConflicted(ctx, p, *land, Conflict{State: LandApplyConflicted, Paths: paths, Reason: reason}, land.By, land.Source)
	}
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
