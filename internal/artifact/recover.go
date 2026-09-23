package artifact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/gopact-ai/steve/internal/artifact/ops"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

// LandRecoveryPending is a landing cut off while applying: the WAL says
// which paths were started; the canonical workspace says which landed.
const LandRecoveryPending = "recovery-pending"

// RecoverLandings is run at boot. A landing that never reached applying
// wrote nothing and is closed as interrupted. One cut off mid-apply is
// marked recovery-pending before anything else is tried — from then on no
// new landing merges onto the half-written workspace — then takes the
// canonical lock again under a new epoch and finishes path by path: a path
// already at the merged content is skipped, one still at the old content
// is rewritten, and anything else is a conflict that ends the landing
// before any path is written.
//
// The previous process is gone, so the canonical lock a landing took for
// itself is released first; a lock it was lent is its lender's, which
// recovery waits for. A landing that still cannot be recovered — the lock
// is someone else's, the home node is unreachable — is logged and left
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
		_, _, projectErr := s.projects.Get(ctx, land.Project)
		if errors.Is(projectErr, project.ErrNotOwner) {
			continue
		}
		dead := land.Lease
		if land.State == LandApplying {
			if err := s.pendRecovery(ctx, &land); err != nil {
				s.noteRecoveryLeft(land, err)
				continue
			}
		}
		if projectErr != nil {
			s.noteRecoveryLeft(land, projectErr)
			continue
		}
		if err := s.releaseDeadLanding(ctx, land, dead); err != nil {
			s.noteRecoveryLeft(land, err)
			continue
		}
		switch land.State {
		case LandProposed, LandLocked, LandMerged:
			if land.Recoverable {
				out = append(out, land)
				continue
			}
			land.Lease = nil
			land.Unapplied = true
			s.failed(ctx, &land, land.State, LandMergeConflicted, "interrupted before apply; nothing was written", nil)
			out = append(out, land)
		case LandRecoveryPending:
			recovered, err := s.recoverLanding(ctx, land)
			if err != nil {
				s.noteRecoveryLeft(recovered, err)
				continue
			}
			if recovered.State != LandRecoveryPending {
				s.recoveryNotes.Delete(land.ID)
				out = append(out, recovered)
			}
		}
	}
	return out, nil
}

// pendRecovery marks a landing cut off mid-apply as waiting for recovery.
// The lock it recorded belonged to the process that was applying it.
func (s *Store) pendRecovery(ctx context.Context, land *Landing) error {
	land.Lease = nil
	return s.move(ctx, land, LandApplying, LandRecoveryPending, nil)
}

// releaseDeadLanding lets go of what the previous process held for a
// landing it did not finish: the driver of a durable landing, and the
// canonical lock the landing took for itself. Only the exact lease the
// landing recorded is released; one taken since is someone else's, and
// one it was lent is still its lender's.
func (s *Store) releaseDeadLanding(ctx context.Context, land Landing, lease *ledger.Lease) error {
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
	if lease != nil && !land.Borrowed && (inFlight || land.Recoverable) {
		if err := s.ledger.ReleaseAny(ctx, *lease); err != nil && !errors.Is(err, ledger.ErrStale) {
			return fmt.Errorf("release canonical lock of the previous process: %w", err)
		}
	}
	return nil
}

// noteRecoveryLeft records a landing whose recovery has to wait, with the
// keys an operator filters on. The periodic retry meets the same obstacle
// every pass; it is logged when it first appears or changes, not each time.
func (s *Store) noteRecoveryLeft(land Landing, err error) {
	if previous, ok := s.recoveryNotes.Swap(land.ID, err.Error()); ok && previous == err.Error() {
		return
	}
	slog.Warn(fmt.Sprintf("artifact: landing %s into %s left %s for a later retry: %v", land.ID, land.Project, land.State, err),
		"landing", land.ID, "project", land.Project, "artifact", land.Artifact, "state", land.State, "error", err.Error())
}

// RetryRecoveries finishes the landings nobody is finishing (see
// awaitingRecovery): ones boot recovery could not finish, ones whose apply
// failed and could not be inspected or not even recorded, or whose
// recovery was cut off in turn. It is run
// periodically. A landing whose canonical lock is busy, or whose own
// driver is running, is skipped quietly until the next pass; any other
// failure is logged and retried next pass. What was recovered — to
// committed or to a conflict — is returned.
func (s *Store) RetryRecoveries(ctx context.Context) ([]Landing, error) {
	pending, err := s.awaitingRecovery(ctx, "")
	if err != nil {
		return nil, err
	}
	var out []Landing
	for _, land := range pending {
		// Still being applied here: its lock is gone, but its writer is
		// not. It stays awaiting recovery, holding new landings off, and
		// is taken over once that apply has returned.
		if _, busy := s.inApply.Load(land.ID); busy {
			continue
		}
		if _, _, err := s.projects.Get(ctx, land.Project); errors.Is(err, project.ErrNotOwner) {
			continue
		} else if err != nil {
			s.noteRecoveryLeft(land, err)
			continue
		}
		recovered, err := s.retryRecovery(ctx, land)
		if errors.Is(err, ledger.ErrHeld) {
			continue
		}
		if err != nil {
			s.noteRecoveryLeft(recovered, err)
			continue
		}
		if recovered.State == LandRecoveryPending {
			continue
		}
		s.recoveryNotes.Delete(land.ID)
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

// recoverLanding recovers a landing nobody is applying any more. What can
// never be recovered ends it: a project that is gone, or one whose
// directory moved — the paths were started in the old one, and writing the
// rest into the new one would be half a landing. Otherwise it waits for
// the project's writer to be free, takes the canonical lock and finishes.
// A landing it finds still recovery-pending — another recovery raced it
// and the record vanished — comes back unchanged.
func (s *Store) recoverLanding(ctx context.Context, land Landing) (Landing, error) {
	if land.State == LandApplying {
		if err := s.pendRecovery(ctx, &land); err != nil {
			return land, err
		}
	}
	p, ok, err := s.projects.GetHistorical(ctx, land.Project)
	if err != nil {
		return land, fmt.Errorf("landing %s: read project %s: %w", land.ID, land.Project, err)
	}
	if !ok {
		land.Lease = nil
		s.conflictedRecovery(ctx, project.Project{}, &land, land.Paths, fmt.Sprintf("project %s no longer exists", land.Project))
		return land, nil
	}
	if land.Target.Path == "" || land.Target != p.Home {
		land.Lease = nil
		s.conflictedRecovery(ctx, p, &land, land.Paths, fmt.Sprintf("the project directory moved from %s to %s while this landing was being written; nothing more was written to the old directory, where it may be partly written, and nothing to the new one. Land it again to write it into the project directory", placeOf(land.Target), placeOf(p.Home)))
		return land, nil
	}
	if err := s.ledger.Update(ctx, func(tx *ledger.Tx) error { return attempt.CheckWriterTx(tx, p.Home.Node, p.Home.Path) }); err != nil {
		return land, err
	}
	// Nothing may be written before the lock is held again.
	// The recovery holds the lock under a name of its own. The landing's
	// own name would re-enter a lock the landing still holds while it
	// recovers its failed apply in place, and the two would run at once.
	lease, err := s.ledger.AcquireIn(ctx, s.homeRegion(ctx, p), "canonical:"+p.ID, landingHolder(ctx, "recovery:"+land.ID+":"+attempt.NewID()), landTTL)
	if err != nil {
		return land, fmt.Errorf("landing %s: %w", land.ID, err)
	}
	defer s.releaseCanonical(ctx, &land, lease)
	defer s.keepCanonical(ctx, lease)()
	return s.finishRecovery(ctx, p, land, lease)
}

// sameLease says whether a recorded lease is the one held: renewals move
// only its expiry.
func sameLease(recorded *ledger.Lease, held ledger.Lease) bool {
	return recorded != nil && recorded.Region == held.Region && recorded.Key == held.Key &&
		recorded.Incarnation == held.Incarnation && recorded.Epoch == held.Epoch && recorded.Holder == held.Holder
}

// placeOf names a project directory, with the machine it is on.
func placeOf(home project.Home) string {
	if home.Node == "" {
		return home.Path
	}
	return home.Node + ":" + home.Path
}

// finishRecovery brings a recovery-pending landing to its end under the
// canonical lock, which lease is. It reads before it writes: every path is
// inspected first, and a recovery that ends in a conflict writes nothing.
func (s *Store) finishRecovery(ctx context.Context, p project.Project, land Landing, lease ledger.Lease) (Landing, error) {
	land.Lease = &lease
	// Another recovery of the same landing may have finished it while this
	// one waited for the lock; its outcome stands.
	if op, found, err := s.ledger.Operation(ctx, land.ID); err != nil {
		return land, err
	} else if !found {
		return land, nil
	} else {
		var stored Landing
		if err := json.Unmarshal(op.Data, &stored); err != nil {
			return land, err
		}
		if op.State != LandRecoveryPending {
			stored.State = op.State
			return stored, nil
		}
		// A lock the recovery took for itself goes on the record before
		// anything is written: if this process dies mid-recovery, boot
		// recovery releases exactly the lock the record names, and the
		// project does not wait out its TTL.
		if !sameLease(stored.Lease, lease) {
			land.Borrowed = false
			if err := s.move(ctx, &land, LandRecoveryPending, LandRecoveryPending, nil); err != nil {
				return land, fmt.Errorf("landing %s: record the recovery's lock: %w", land.ID, err)
			}
		}
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
	// would read as "old" and be written into the user's repository. It is
	// refused before anything is inspected, as a new landing refuses it.
	if inside, repos := pathsInsideNested(land.Paths, nested); len(inside) > 0 {
		written, _, _, _ := s.inspectPaths(ctx, p, land, outside(land.Paths, inside))
		s.conflictedRecovery(ctx, p, &land, inside, nestedReason(repos)+partlyWritten(written, len(land.Paths)))
		return land, nil
	}
	written, stale, conflicted, err := s.inspectPaths(ctx, p, land, land.Paths)
	if err != nil {
		return land, err
	}
	if len(conflicted) > 0 {
		s.conflictedRecovery(ctx, p, &land, conflicted, "these paths were changed by someone else while the landing was being written, or it turns them between files and directories, which cannot be finished path by path"+partlyWritten(written, len(land.Paths)))
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
			// The result has landed now. A queue entry for it — the pass
			// that started this landing stopped at recovery-pending and
			// left it queued — would only land it again, as nothing.
			if _, err := tx.Exec(`DELETE FROM bindings WHERE kind = ? AND id = ?`, pendingKind, p.ID+"/"+land.Artifact); err != nil {
				return err
			}
			return tx.SetData(op, land)
		})
	if err != nil {
		land.State = LandRecoveryPending
		s.failed(ctx, &land, LandRecoveryPending, LandCommitConflict, err.Error(), land.Paths)
		return land, nil
	}
	if _, err := s.receipt(ctx, p, Manifest{ID: land.Merged, Project: p.ID, Parent: land.Now, Label: p.Level, By: land.ID, Message: "landed " + short(land.Artifact) + " (recovered)", Canonical: true}); err != nil {
		return land, err
	}
	return land, nil
}

// inspectPaths sorts a landing's paths by what the canonical workspace
// holds: already at the merged content, still at the old content, or
// something else. It stops at the first path it cannot inspect, returning
// what it sorted so far.
func (s *Store) inspectPaths(ctx context.Context, p project.Project, land Landing, paths []string) (written, stale, conflicted []string, err error) {
	for _, path := range paths {
		state, err := s.pathState(ctx, p, land, path)
		if err != nil {
			return written, stale, conflicted, err
		}
		switch state {
		case "merged":
			written = append(written, path)
		case "old":
			stale = append(stale, path)
		default:
			conflicted = append(conflicted, path)
		}
	}
	return written, stale, conflicted, nil
}

// outside is paths without those in skip.
func outside(paths, skip []string) []string {
	skipped := make(map[string]bool, len(skip))
	for _, path := range skip {
		skipped[path] = true
	}
	var out []string
	for _, path := range paths {
		if !skipped[path] {
			out = append(out, path)
		}
	}
	return out
}

// partlyWritten says, for a recovery that stops, how much of the landing
// the interrupted write had already put in the workspace, where it stays.
func partlyWritten(written []string, total int) string {
	if len(written) == 0 {
		return ""
	}
	return fmt.Sprintf("; %d of its %d paths had already been written before it stopped and are left as they are (%s)", len(written), total, clipPaths(written, 5))
}

// clipPaths lists a few paths and says how many more there are.
func clipPaths(paths []string, n int) string {
	if len(paths) <= n {
		return strings.Join(paths, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(paths[:n], ", "), len(paths)-n)
}

// conflictedRecovery ends a recovery that wrote nothing as apply-conflicted
// on paths, and keeps a queued result blocked on it the way a landing's own
// conflict is kept, with the reason a person can act on. A project that
// is gone has no queue to keep it on.
func (s *Store) conflictedRecovery(ctx context.Context, p project.Project, land *Landing, paths []string, reason string) {
	s.failed(ctx, land, LandRecoveryPending, LandApplyConflicted, "recovery: "+reason, paths)
	if land.State == LandApplyConflicted && !land.Recoverable && p.ID != "" {
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
