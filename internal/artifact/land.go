package artifact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/artifact/ops"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

// Landing is the operation that brings an artifact into the project's
// canonical workspace.
//
//	proposed → locked → merged → applying → committed
//	locked   → merge-conflicted     (both sides changed the same paths)
//	locked   → apply-conflicted     (paths fall inside a nested repository)
//	applying → apply-conflicted     (a path changed under the WAL)
//	applying → recovery-pending     (the apply or the commit failed; see recover.go)
//
// The commit moves the canonical name from the snapshot the landing merged
// onto to the merged one. Only a holder of the canonical lock moves the
// name, so it is still there; should it not be, the fully written landing
// is not failed for it but waits for recovery, saying why, and recovery
// commits it against a fresh snapshot, as it does any landing whose commit
// did not go through.
//
// A landing's record is opened once the canonical lock is held: a lock
// someone else holds is contention, returned as ledger.ErrHeld, and leaves
// no record behind. The lock is held from locked to the end. Before
// applying, the merged snapshot is put where the canonical workspace
// lives, which is where recovery reads it. Every path written is journaled
// as an effect — started before, confirmed after — under the WAL round, so
// a landing cut off mid-apply is recovered per path rather than guessed at.
// Source keeps an execution's original permission with its result.
type Source struct {
	Execution *task.ExecutionToken `json:"execution"`
	AttemptID string               `json:"attempt_id"`
}

// SourceOf attaches the currently executing task's authorization to a result.
func SourceOf(ctx context.Context, attemptID string) []Source {
	if token := execution.Token(ctx); token != nil {
		return []Source{{Execution: token, AttemptID: attemptID}}
	}
	return nil
}

func firstSource(s []Source) *Source {
	if len(s) == 0 {
		return nil
	}
	origin := s[0]
	return &origin
}

type Landing struct {
	Target         project.Home `json:"target"`
	writer         project.Home
	borrowedHolder string
	Source         *Source `json:"source,omitempty"`
	ID             string  `json:"id"`
	Project        string  `json:"project"`
	Artifact       string  `json:"artifact"`
	Base           string  `json:"base,omitempty"`
	Now            string  `json:"now,omitempty"`
	Merged         string  `json:"merged,omitempty"`
	// Conflict names the half-merged snapshot kept when the two sides
	// disagreed: the canonical side and the incoming side with conflict
	// markers in the files they both touched. It is what a person or an
	// agent works from to resolve the landing.
	Conflict    string        `json:"conflict,omitempty"`
	By          string        `json:"by,omitempty"`
	State       string        `json:"state"`
	Paths       []string      `json:"paths,omitempty"`
	Round       int           `json:"round"`
	Error       string        `json:"error,omitempty"`
	Lease       *ledger.Lease `json:"lease,omitempty"`
	StartedAt   time.Time     `json:"started_at"`
	EndedAt     time.Time     `json:"ended_at,omitempty"`
	Recoverable bool          `json:"recoverable,omitempty"`
	// Borrowed says Lease is the caller's, lent for this landing: a parent
	// turn's lock, which outlives the landing and is never released by it.
	Borrowed bool `json:"borrowed,omitempty"`
	// Unapplied is durably recorded only when closing a preapply state.
	Unapplied bool `json:"unapplied,omitempty"`
}

const (
	LandProposed        = "proposed"
	LandLocked          = "locked"
	LandMerged          = "merged"
	LandApplying        = "applying"
	LandCommitted       = "committed"
	LandMergeConflicted = "merge-conflicted"
	LandApplyConflicted = "apply-conflicted"
	landKind            = "landing"
	landTTL             = 5 * time.Minute
)

// Conflict is a landing that stopped at a conflict, with the paths and,
// for a merge conflict git could keep a tree for, the marked snapshot.
// Reason says in words why an apply conflict stopped.
type Conflict struct {
	State  string
	Paths  []string
	Marked string
	Reason string
}

func (c Conflict) Error() string {
	if c.Reason != "" {
		return fmt.Sprintf("landing %s: %s: %s", c.State, c.Reason, strings.Join(c.Paths, ", "))
	}
	return fmt.Sprintf("landing %s: %s", c.State, strings.Join(c.Paths, ", "))
}

// ErrRecoveryPending refuses a new landing while an earlier one on the
// same project is still waiting to be recovered: the canonical workspace
// is half written, and a snapshot of it is nothing to merge onto.
var ErrRecoveryPending = errors.New("an interrupted landing on this project is waiting to be recovered")

// Land merges an artifact into the project's canonical workspace. The
// caller must not hold the canonical lock: the landing takes it, and
// ErrHeld comes back as ledger.ErrHeld when someone else has it.
func (s *Store) Land(ctx context.Context, p project.Project, artifactID, by string, source ...Source) (Landing, error) {
	return s.land(ctx, p, artifactID, by, nil, firstSource(source), nil)
}

// LandUnder lands while someone else legitimately holds the canonical lock
// and asked for it: a parent turn in place, taking a delegate's result into
// its own directory. The landing is fenced on that lease and releases
// nothing.
func (s *Store) LandUnder(ctx context.Context, p project.Project, artifactID, by string, held ledger.Lease, source ...Source) (Landing, error) {
	return s.land(ctx, p, artifactID, by, &held, firstSource(source), nil)
}

// land runs one landing from admission to commit, in the order the state
// machine above spells out. held is a canonical lock the caller lends;
// resume is the record of a landing that recovery is finishing, which
// keeps its identity and never records a failure of its own.
func (s *Store) land(ctx context.Context, p project.Project, artifactID, by string, held *ledger.Lease, source *Source, resume *Landing) (land Landing, err error) {
	borrowedHolder, err := s.checkLandingWriter(ctx, p, held)
	if err != nil {
		return Landing{}, err
	}
	ctx, finish, err := s.admitLandingSource(ctx, artifactID, source)
	if err != nil {
		return Landing{}, err
	}
	defer finish()
	land, err = s.proposeLanding(ctx, p, artifactID, by, borrowedHolder, source, resume)
	if err != nil {
		return Landing{}, err
	}
	defer func() {
		if err != nil && !land.Recoverable {
			s.closeUnappliedLanding(ctx, &land, err)
		}
	}()
	unlock, err := s.lockCanonical(ctx, p, &land, held)
	if err != nil {
		return land, err
	}
	defer unlock()
	// A lent lock is still its lender's to snapshot under: the slot keeps
	// the lender off the workspace from the snapshot merged onto to the
	// commit.
	done, err := s.writeCanonical(ctx, p.ID)
	if err != nil {
		return land, err
	}
	defer done()
	if err := s.checkNoRecoveryPending(ctx, p, land.ID); err != nil {
		return land, err
	}
	if resume == nil {
		if _, err := s.ledger.Begin(ctx, land.ID, landKind, LandProposed, by, land); err != nil {
			return land, err
		}
	}
	if err := s.move(ctx, &land, LandProposed, LandLocked, nil); err != nil {
		return land, err
	}
	// A conflict is kept on the queue, merge or apply alike: retrying it
	// against the same canonical would only reach it again.
	defer func() {
		var conflict Conflict
		if errors.As(err, &conflict) && !land.Recoverable {
			s.queueConflicted(ctx, p, land, conflict, by, source)
		}
	}()
	repo, err := s.mergeLanding(ctx, p, &land)
	if err != nil {
		return land, err
	}
	if err := s.move(ctx, &land, LandLocked, LandMerged, nil); err != nil {
		return land, err
	}
	if len(land.Paths) == 0 {
		// Nothing to write: the canonical already has it all.
		if err := s.move(ctx, &land, LandMerged, LandCommitted, nil); err != nil {
			return land, err
		}
		return land, nil
	}
	if err := s.stageMerged(ctx, p, &land, repo); err != nil {
		return land, err
	}
	land.Round++
	defer s.applying(land.ID)()
	if err := s.move(ctx, &land, LandMerged, LandApplying, nil); err != nil {
		return land, err
	}
	// Applying is already admitted under the source epoch. Finish this WAL
	// operation even if a later stop revokes permission for new work.
	applyCtx, finishApply := landingApplyContext(ctx)
	defer finishApply()
	ctx = applyCtx
	if err := s.applyLanding(ctx, p, &land, repo); err != nil {
		return land, err
	}
	if land.State == LandCommitted {
		// The apply failed part-way and recovering it finished the landing.
		return land, nil
	}
	if err := s.commitLanding(ctx, p, &land); err != nil {
		return land, err
	}
	return land, nil
}

// checkLandingWriter is who may write the canonical workspace: a lock the
// caller lends must be the project's and still held, and the home must
// accept its holder as the writer — or, with no lock lent, be unheld.
func (s *Store) checkLandingWriter(ctx context.Context, p project.Project, held *ledger.Lease) (borrowedHolder string, err error) {
	if held != nil {
		if held.Key != canonicalLock(p.ID) {
			return "", fmt.Errorf("landing lease %s does not own project %s", held.Key, p.ID)
		}
		if err := s.ledger.CheckAny(ctx, *held); err != nil {
			return "", err
		}
		borrowedHolder = held.Holder
	}
	if err := s.ledger.Update(ctx, func(tx *ledger.Tx) error { return attempt.CheckWriterTx(tx, p.Home.Node, p.Home.Path, borrowedHolder) }); err != nil {
		return "", err
	}
	return borrowedHolder, nil
}

// admitLandingSource admits a delegated result under the execution it came
// from: the landing runs inside an accepted scope of that task, which the
// returned finish closes once the landing is over. A landing with no
// source, or a store with no execution registry, gets its context back as
// it was and a finish that does nothing.
func (s *Store) admitLandingSource(ctx context.Context, artifactID string, source *Source) (context.Context, func(), error) {
	finish := func() {}
	if source == nil {
		return ctx, finish, nil
	}
	if source.Execution == nil {
		return ctx, finish, errors.New("landing source requires execution authorization")
	}
	if s.executions != nil {
		scope, err := s.executions.BeginAccepted(ctx, execution.Key{TaskID: source.Execution.TaskID, InstanceID: "land/" + artifactID, AttemptID: source.AttemptID}, source.Execution)
		if err != nil {
			return ctx, finish, err
		}
		finish = func() { scope.Finish(nil) }
		ctx = scope.Context()
	}
	if err := s.ledger.Update(ctx, func(tx *ledger.Tx) error { return task.CheckExecutionTx(tx, source.Execution) }); err != nil {
		finish()
		return ctx, func() {}, err
	}
	return ctx, finish, nil
}

// proposeLanding resolves the artifact and the canonical snapshot it
// descends from, which is the merge base. A durable caller's landing opens
// its record here, under the identity it keeps across retries; recovery
// resuming one keeps its record, identity and start. Any other landing's
// record is opened by land once the canonical lock is held.
func (s *Store) proposeLanding(ctx context.Context, p project.Project, artifactID, by, borrowedHolder string, source *Source, resume *Landing) (Landing, error) {
	m, ok, err := s.Manifest(ctx, artifactID)
	if err != nil {
		return Landing{}, err
	}
	if !ok {
		return Landing{}, fmt.Errorf("artifact %s is unknown", short(artifactID))
	}
	if !m.Durable(p) {
		return Landing{}, fmt.Errorf("artifact %s is not durable yet; it cannot land", short(artifactID))
	}
	base, err := s.canonicalAncestor(ctx, m)
	if err != nil {
		return Landing{}, err
	}
	land := Landing{borrowedHolder: borrowedHolder, writer: p.Home, Target: p.Home, Source: source, ID: "land-" + short(artifactID) + "-" + fmt.Sprint(s.now().UnixNano()), Project: p.ID, Artifact: artifactID, Base: base, By: by, State: LandProposed, StartedAt: s.now().UTC()}
	if resume != nil {
		land.ID, land.Recoverable = resume.ID, true
		if !resume.StartedAt.IsZero() {
			land.StartedAt = resume.StartedAt
		}
	}
	if resume != nil && resume.State == "" {
		if _, err := s.ledger.Begin(ctx, land.ID, landKind, LandProposed, by, land); err != nil {
			return Landing{}, err
		}
	}
	return land, nil
}

// checkNoRecoveryPending refuses to land onto a canonical workspace that
// an earlier, interrupted landing has half written and not yet recovered:
// one recovery-pending, or one left applying by a writer that lost its
// lock. It is asked under the canonical lock, which a recovery in progress
// holds.
func (s *Store) checkNoRecoveryPending(ctx context.Context, p project.Project, self string) error {
	pending, err := s.awaitingRecovery(ctx, p.ID)
	if err != nil {
		return err
	}
	for _, other := range pending {
		if other.ID == self {
			continue
		}
		return fmt.Errorf("%w: landing %s into %s", ErrRecoveryPending, other.ID, p.ID)
	}
	return nil
}

// awaitingRecovery lists the landings nobody is finishing: those marked
// recovery-pending, and those still applying whose canonical lock is
// gone. The second kind is what a failed apply leaves when even recording
// it for recovery was refused — the lock was lost underneath it — and,
// unlisted, it would neither hold new landings off the half-written
// workspace nor ever be retried. One whose lock cannot be checked is not
// taken for dead. projectID narrows the list to one project ("" is all)
// before any lock is checked.
func (s *Store) awaitingRecovery(ctx context.Context, projectID string) ([]Landing, error) {
	var out []Landing
	for _, state := range []string{LandRecoveryPending, LandApplying} {
		ops, err := s.ledger.Operations(ctx, landKind, state)
		if err != nil {
			return nil, err
		}
		for _, op := range ops {
			var land Landing
			if err := json.Unmarshal(op.Data, &land); err != nil {
				continue
			}
			land.ID, land.State = op.ID, op.State
			if projectID != "" && land.Project != projectID {
				continue
			}
			if state == LandApplying && land.Lease != nil {
				if err := s.ledger.CheckAny(ctx, *land.Lease); !errors.Is(err, ledger.ErrStale) {
					continue
				}
			}
			out = append(out, land)
		}
	}
	return out, nil
}

// queueConflicted keeps a result that could not be merged or applied,
// along with what resolving it needs. A conflict used to end the landing and, for a result
// that was landing directly rather than from the queue, end the result:
// nothing held it, so nothing ever retried or resolved it. Now both paths
// leave the same record, and the queue is what carries it forward.
func (s *Store) queueConflicted(ctx context.Context, p project.Project, land Landing, conflict Conflict, by string, source *Source) {
	// Recorded on the way out of the landing, after its own contexts may
	// have ended; the canonical it is blocked on must still be read.
	ctx = context.WithoutCancel(ctx)
	id := p.ID + "/" + land.Artifact
	err := s.ledger.Update(ctx, func(tx *ledger.Tx) error {
		canonical, _, err := tx.Name(CanonicalRef(p.ID))
		if err != nil {
			return fmt.Errorf("read canonical of %s: %w", p.ID, err)
		}
		blocked := &Blocked{Landing: land.ID, State: conflict.State, Canonical: canonical.Artifact, Marked: conflict.Marked, Paths: conflict.Paths, Reason: conflict.Reason, At: s.now().UTC()}
		raw, err := tx.Bindings(pendingKind)
		if err != nil {
			return err
		}
		// An entry already queued keeps its own place in line and its own
		// source authority; only why it is stuck is new.
		item := Pending{Project: p.ID, Artifact: land.Artifact, By: by, At: s.now().UTC(), Source: source}
		if existing, ok := raw[id]; ok {
			var prior Pending
			if err := json.Unmarshal(existing, &prior); err == nil && prior.Project == p.ID {
				item = prior
			}
		}
		item.Blocked = blocked
		return tx.PutBinding(pendingKind, id, item)
	})
	if err != nil {
		slog.Warn(fmt.Sprintf("artifact: queue conflicted result %s: %v", short(land.Artifact), err), "landing", land.ID, "artifact", land.Artifact, "project", land.Project)
	}
}

// lockCanonical takes the project's canonical lock for the landing, or
// adopts the one the caller lends, which is released by nobody here. A
// lock of the landing's own is kept alive while the landing works (see
// keepCanonical), and the returned unlock stops that and gives it back. A lock
// someone else holds comes back as it is: contention, not a conflict.
func (s *Store) lockCanonical(ctx context.Context, p project.Project, land *Landing, held *ledger.Lease) (unlock func(), err error) {
	if held != nil {
		lease := *held
		land.Lease, land.Borrowed = &lease, true
		return func() {}, nil
	}
	lease, err := s.acquireCanonical(ctx, p, landingHolder(ctx, land.ID))
	if err != nil {
		return nil, err
	}
	land.Lease = &lease
	untrack := s.keepCanonical(ctx, lease)
	return func() {
		untrack()
		s.releaseCanonical(ctx, land, lease)
	}, nil
}

// mergeLanding, under the lock, snapshots what the canonical workspace
// holds right now and three-way merges the artifact onto it from their
// common base, recording the merged snapshot and the paths it changes.
// Paths both sides changed end the landing as merge-conflicted; paths
// inside a nested repository the snapshot left out end it apply-conflicted
// before anything is written. The project's repository comes back for
// applying.
func (s *Store) mergeLanding(ctx context.Context, p project.Project, land *Landing) (*Repo, error) {
	now, nested, err := s.snapshotUnderLanding(ctx, p, *land.Lease, land.ID, "before landing "+short(land.Artifact))
	if err != nil {
		if !land.Recoverable {
			s.failed(ctx, land, LandLocked, LandMergeConflicted, "snapshot: "+err.Error(), nil)
		}
		return nil, err
	}
	land.Now = now.ID
	repo, err := s.Repo(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	var merged, marked string
	var conflicts []string
	if metadataOnly(p) {
		merged, marked, conflicts, err = s.mergeOnNode(ctx, p, land.Base, now.ID, land.Artifact)
	} else {
		merged, marked, conflicts, err = repo.MergeMarking(ctx, land.Base, now.ID, land.Artifact, "land "+short(land.Artifact)+" into "+p.ID)
	}
	if err != nil {
		if !land.Recoverable {
			s.failed(ctx, land, LandLocked, LandMergeConflicted, err.Error(), nil)
		}
		return nil, err
	}
	if len(conflicts) > 0 {
		// The half-merged snapshot is only useful if it can be worked in,
		// and a workspace may only be materialized from an artifact whose
		// lineage is known. Record it as one, parented on the canonical
		// snapshot it was built against.
		if marked != "" {
			if _, err := s.receipt(ctx, p, Manifest{ID: marked, Project: p.ID, Parent: now.ID, Label: p.Level, By: land.ID, Message: "conflict landing " + short(land.Artifact)}); err != nil {
				slog.Warn(fmt.Sprintf("artifact: record conflicted snapshot %s: %v", short(marked), err), "landing", land.ID, "artifact", land.Artifact, "project", land.Project)
				marked = ""
			}
		}
		land.Conflict = marked
		s.failed(ctx, land, LandLocked, LandMergeConflicted, "conflicts", conflicts)
		return nil, Conflict{State: LandMergeConflicted, Paths: conflicts, Marked: marked}
	}
	land.Merged = merged
	paths, err := s.changedBetween(ctx, p, now.ID, merged)
	if err != nil {
		return nil, err
	}
	if inside, repos := pathsInsideNested(paths, nested); len(inside) > 0 {
		reason := nestedReason(repos)
		land.Unapplied = true
		s.failed(ctx, land, LandLocked, LandApplyConflicted, reason, inside)
		return nil, Conflict{State: LandApplyConflicted, Paths: inside, Reason: reason}
	}
	land.Paths = paths
	return repo, nil
}

// pathsInsideNested picks the paths that fall inside one of the nested
// repositories, and which repositories they fall in. Both lists are
// slash-separated and relative to the canonical workspace. Case is
// ignored: on a case-insensitive file system, as macOS has by default,
// Inner/x is written into inner/. The home's file system is not asked, so
// on a case-sensitive one a path differing from a nested repository only
// in case is refused too; refusing is the side that loses no data.
func pathsInsideNested(paths, nested []string) (inside, repos []string) {
	hit := map[string]bool{}
	for _, path := range paths {
		for _, dir := range nested {
			if strings.EqualFold(path, dir) || len(path) > len(dir) && path[len(dir)] == '/' && strings.EqualFold(path[:len(dir)], dir) {
				inside = append(inside, path)
				if !hit[dir] {
					hit[dir] = true
					repos = append(repos, dir)
				}
				break
			}
		}
	}
	return inside, repos
}

// nestedReason says why a landing into nested repositories is refused, in
// terms of what a person can do about it.
func nestedReason(repos []string) string {
	noun := "a nested git repository"
	if len(repos) > 1 {
		noun = "nested git repositories"
	}
	return fmt.Sprintf("the result writes inside %s (%s) of the project directory; snapshots leave nested repositories out, so these files cannot be merged with what is on disk. Make the directory an ordinary one (without its own .git) or move it out of the project, then land again", noun, strings.Join(repos, ", "))
}

// applyLanding writes the merged tree into the canonical workspace, every
// path journaled before and confirmed after, so a landing cut off here is
// recovered per path. A write that fails may have written some paths
// first, so it is recovered the same way, at once, durable or not.
func (s *Store) applyLanding(ctx context.Context, p project.Project, land *Landing, repo *Repo) error {
	journal := s.ledger.Journal()
	for _, path := range land.Paths {
		if _, err := journal.Started(landPathEffect(*land, path), "", nil); err != nil {
			return err
		}
	}
	var err error
	if p.Home.Node == "" {
		_, err = repo.Apply(ctx, land.Now, land.Merged, p.Home.Path)
	} else {
		err = s.applyOnNode(ctx, p, land.Now, land.Merged)
	}
	if err != nil {
		return s.recoverFailedApply(ctx, p, land, err)
	}
	for _, path := range land.Paths {
		if _, err := journal.Confirmed(landPathEffect(*land, path), nil); err != nil {
			return err
		}
	}
	return nil
}

// recoverFailedApply settles a landing whose apply failed. Whether the
// apply wrote nothing — the working tree refused it — or stopped part-way,
// only the workspace can say, so the landing is recovered path by path
// under the lock it already holds: a path someone else changed ends it
// apply-conflicted on that path with nothing further written, and one the
// failed apply simply did not reach is written now. What cannot be
// inspected yet — the home node is gone — is left recovery-pending, which
// keeps new landings off the half-written workspace until the periodic
// retry finishes it.
func (s *Store) recoverFailedApply(ctx context.Context, p project.Project, land *Landing, applyErr error) error {
	slog.Warn(fmt.Sprintf("artifact: landing %s into %s: apply failed, recovering it path by path: %v", land.ID, land.Project, applyErr),
		"landing", land.ID, "project", land.Project, "artifact", land.Artifact, "error", applyErr.Error())
	if err := s.move(context.WithoutCancel(ctx), land, LandApplying, LandRecoveryPending, map[string]any{"apply_error": applyErr.Error()}); err != nil {
		return fmt.Errorf("apply: %w; recording it for recovery: %v", applyErr, err)
	}
	recovered, err := s.finishRecovery(ctx, p, *land, *land.Lease)
	if recovered.ID != "" {
		*land = recovered
	}
	if err != nil || land.State == LandRecoveryPending {
		if err == nil {
			// finishRecovery logged why its outcome was not recorded.
			err = errors.New("its outcome could not be recorded")
		}
		return fmt.Errorf("%w: landing %s: apply failed (%v) and recovering it failed: %v", ErrRecoveryPending, land.ID, applyErr, err)
	}
	if land.State != LandCommitted {
		return Conflict{State: land.State, Paths: land.Paths, Reason: strings.TrimPrefix(land.Error, "recovery: ")}
	}
	return nil
}

// landPathEffect identifies one path's write in the landing's current
// round; recovery reads the same identity back from the WAL.
func landPathEffect(land Landing, path string) ledger.EffectID {
	return ledger.EffectID{Operation: land.ID, Kind: "land-path", InstanceKey: fmt.Sprintf("%d/%s", land.Round, path)}
}

// snapshotUnderLanding snapshots the canonical workspace under the lock a
// landing or its recovery holds, parented on the canonical name read under
// that lock, and moves the name to it.
func (s *Store) snapshotUnderLanding(ctx context.Context, p project.Project, held ledger.Lease, by, message string) (Manifest, []string, error) {
	parent, err := s.CanonicalOf(ctx, p.ID)
	if err != nil {
		return Manifest{}, nil, err
	}
	m, _, nested, err := s.snapshotCanonical(ctx, p, held, parent, by, message)
	return m, nested, err
}

// commitLanding moves the canonical name from the snapshot the landing
// merged onto to the merged snapshot, in the same transaction as the
// committed state, fenced on the lock, then signs for the merged snapshot
// as the project's new canonical. A commit that does not go through —
// the name is not where the landing left it, or it cannot be read — leaves
// the written landing recovery-pending, for recovery to commit.
//
// Under a lock lent by an in-place turn, the name moves to the merged
// snapshot although the lender may have written outside the landing's
// paths, so the name can briefly lag what is on disk. The lender's turn
// ends with a snapshot cut from the workspace as it is, which moves the
// name to it and closes the gap.
func (s *Store) commitLanding(ctx context.Context, p project.Project, land *Landing) error {
	committed := *land
	committed.State = LandCommitted
	committed.EndedAt = s.now().UTC()
	_, err := s.ledger.Transition(ctx, land.ID, LandApplying, LandCommitted, land.By, landingFence(ctx, []ledger.Lease{*land.Lease}), map[string]any{"paths": land.Paths},
		func(tx *ledger.Tx, op *ledger.Operation) error {
			if err := moveCanonical(tx, p.ID, land.Now, land.Merged); err != nil {
				return err
			}
			return tx.SetData(op, committed)
		})
	if err != nil {
		s.pendCommit(ctx, land, LandApplying, err)
		return fmt.Errorf("%w: landing %s is written; its commit: %w", ErrRecoveryPending, land.ID, err)
	}
	*land = committed
	_, err = s.receipt(ctx, p, Manifest{ID: land.Merged, Project: p.ID, Parent: land.Now, Label: p.Level, By: land.ID, Message: "landed " + short(land.Artifact), Canonical: true})
	return err
}

// pendCommit records a landing written but not committed as waiting for
// recovery, which commits it, and why. A record that cannot be written
// leaves the landing as the record has it: applying with its lock gone,
// which recovery finds as well.
func (s *Store) pendCommit(ctx context.Context, land *Landing, from string, cause error) {
	slog.Warn(fmt.Sprintf("artifact: landing %s into %s is written but not committed; left for recovery: %v", land.ID, land.Project, cause),
		"landing", land.ID, "project", land.Project, "artifact", land.Artifact, "error", cause.Error())
	previous := *land
	land.Error = "written, not committed: " + cause.Error()
	if err := s.move(context.WithoutCancel(ctx), land, from, LandRecoveryPending, map[string]any{"commit_error": cause.Error()}); err != nil {
		*land = previous
		slog.Warn(fmt.Sprintf("artifact: landing %s: why it waits for recovery not recorded: %v", land.ID, err), "landing", land.ID, "project", land.Project)
	}
}

// moveCanonical moves the project's canonical name to merged, inside a
// landing's commit. onto is the canonical snapshot the landing took under
// its lock and wrote relative to; the name must still be there. A name
// found anywhere else is a conflict with the commit.
func moveCanonical(tx *ledger.Tx, projectID, onto, merged string) error {
	current, found, err := tx.Name(CanonicalRef(projectID))
	if err != nil {
		return fmt.Errorf("read canonical of %s: %w", projectID, err)
	}
	if !found || current.Artifact != onto {
		at := "absent"
		if found {
			at = short(current.Artifact)
		}
		return fmt.Errorf("%w: canonical of %s is %s, not %s, which the landing wrote onto", ledger.ErrConflict, projectID, at, short(onto))
	}
	if onto == merged {
		return nil
	}
	_, err = tx.CompareAndSetName(CanonicalRef(projectID), current.Version, merged)
	return err
}

// mergeOnNode three-way merges at the home node of a sealed project, which
// needs the node's git to know merge-tree --write-tree (2.38+).
func (s *Store) mergeOnNode(ctx context.Context, p project.Project, base, ours, theirs string) (string, string, []string, error) {
	version, _, state, err := s.nodes.Git(ctx, p.Home.Node)
	if err != nil {
		return "", "", nil, err
	}
	if ours == base {
		return theirs, "", nil, nil
	}
	if theirs == base {
		return ours, "", nil, nil
	}
	bare := nodeBare(state, p.ID)
	result, err := s.nodes.Artifact(ctx, p.Home.Node, ops.Request{
		Op: ops.Merge, Repo: bare, Base: base, Ours: ours, Theirs: theirs,
		Message:     "land " + short(theirs) + " into " + p.ID,
		LegacyMerge: s.LegacyMerge || !ops.GitAtLeast(version, 2, 38),
	})
	if err != nil {
		var conflict MergeConflict
		if errors.As(err, &conflict) {
			return "", conflict.Marked, conflict.Paths, nil
		}
		return "", "", nil, fmt.Errorf("merge on %s: %w", p.Home.Node, err)
	}
	return result.Commit, "", nil, nil
}

func (s *Store) changedOnNode(ctx context.Context, p project.Project, from, to string) ([]string, error) {
	_, _, state, err := s.nodes.Git(ctx, p.Home.Node)
	if err != nil {
		return nil, err
	}
	result, err := s.nodes.Artifact(ctx, p.Home.Node, ops.Request{Op: ops.Changed, Repo: nodeBare(state, p.ID), From: from, Commit: to})
	return result.Paths, err
}

// stageMerged puts the merged snapshot where the canonical workspace
// lives before the landing starts applying: applying reads it there, and
// so does recovery of a landing cut off while applying. A hub workspace
// reads the hub's repository, where the merge was made; a sealed project
// was merged at its home.
func (s *Store) stageMerged(ctx context.Context, p project.Project, land *Landing, hub *Repo) error {
	if p.Home.Node == "" || metadataOnly(p) {
		return nil
	}
	_, _, state, err := s.nodes.Git(ctx, p.Home.Node)
	if err != nil {
		return err
	}
	bare := nodeBare(state, p.ID)
	if s.nodeHas(ctx, p.Home.Node, bare, land.Merged) {
		return nil
	}
	if err := s.push(ctx, p.Home.Node, bare, hub, land.Merged, []string{land.Now}); err != nil {
		return fmt.Errorf("stage merged snapshot %s on %s: %w", short(land.Merged), p.Home.Node, err)
	}
	if !s.nodeHas(ctx, p.Home.Node, bare, land.Merged) {
		return fmt.Errorf("merged snapshot %s did not arrive at %s", short(land.Merged), p.Home.Node)
	}
	return nil
}

// applyOnNode writes the merged tree into a canonical workspace that lives
// on a node, where stageMerged put it, with the same two-tree merge the
// hub uses.
func (s *Store) applyOnNode(ctx context.Context, p project.Project, from, merged string) error {
	_, _, state, err := s.nodes.Git(ctx, p.Home.Node)
	if err != nil {
		return err
	}
	_, err = s.nodes.Artifact(ctx, p.Home.Node, ops.Request{Op: ops.Apply, Repo: nodeBare(state, p.ID), From: from, Commit: merged, WorkTree: p.Home.Path})
	return err
}

// move records the landing's transition from one state to another. The
// landing in hand keeps its previous state when the transition is refused.
func (s *Store) move(ctx context.Context, land *Landing, from, to string, effects any) (err error) {
	previous := land.State
	land.State = to
	defer func() {
		if err != nil {
			land.State = previous
		}
	}()
	var fencings []ledger.Lease
	if land.Lease != nil {
		fencings = []ledger.Lease{*land.Lease}
	}
	_, err = s.ledger.Transition(ctx, land.ID, from, to, land.By, landingFence(ctx, fencings), effects,
		func(tx *ledger.Tx, op *ledger.Operation) error {
			if to == LandApplying || (to == LandCommitted && from != LandApplying && from != LandRecoveryPending) {
				if err := project.CheckHomeTx(tx, land.Project, land.Target); err != nil {
					return err
				}
			}
			if to == LandApplying {
				if err := attempt.CheckWriterTx(tx, land.writer.Node, land.writer.Path, land.borrowedHolder); err != nil {
					return err
				}
			}
			if land.Source != nil && (to == LandApplying || (to == LandCommitted && from != LandApplying && from != LandRecoveryPending)) {
				if err := task.CheckExecutionTx(tx, land.Source.Execution); err != nil {
					return err
				}
			}
			return tx.SetData(op, *land)
		})
	return err
}

// fail ends a landing in a failure state. A failure that cannot be
// recorded leaves the landing in hand as the record has it.
func (s *Store) fail(ctx context.Context, land *Landing, from, to, cause string, paths []string) error {
	previous := *land
	land.Error = cause
	if paths != nil {
		land.Paths = paths
	}
	land.EndedAt = s.now().UTC()
	if err := s.move(context.WithoutCancel(ctx), land, from, to, map[string]any{"paths": paths}); err != nil {
		*land = previous
		return err
	}
	return nil
}

// failed records a failure whose cause the caller is already returning. A
// record that cannot be written (the lock or driver was lost underneath)
// leaves the landing in its previous state for recovery to find, so it is
// logged rather than allowed to hide the cause.
func (s *Store) failed(ctx context.Context, land *Landing, from, to, cause string, paths []string) {
	if err := s.fail(ctx, land, from, to, cause, paths); err != nil {
		slog.Error(fmt.Sprintf("artifact: landing %s: %s not recorded: %v", land.ID, to, err), "landing", land.ID, "artifact", land.Artifact, "project", land.Project)
	}
}

// releaseCanonical gives the project's canonical lock back once a landing
// is over, on a context the caller's cancellation cannot reach. The
// landing's own result stands whether or not the release succeeds; a lock
// that stays falls to its TTL, and every landing and in-place turn on the
// project waits that long, which is worth a line in the log.
func (s *Store) releaseCanonical(ctx context.Context, land *Landing, lease ledger.Lease) {
	if err := s.ledger.ReleaseAny(context.WithoutCancel(ctx), lease); err != nil {
		slog.Warn(fmt.Sprintf("artifact: landing %s: release canonical lock: %v", land.ID, err), "landing", land.ID, "artifact", land.Artifact, "project", land.Project)
	}
}

// canonicalAncestor walks an artifact's parents to the canonical snapshot
// it descends from: the merge base for landing it.
func (s *Store) canonicalAncestor(ctx context.Context, m Manifest) (string, error) {
	seen := 0
	for cur := m; cur.Parent != "" && seen < 1000; seen++ {
		parent, ok, err := s.Manifest(ctx, cur.Parent)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("artifact %s: parent %s is unknown", short(cur.ID), short(cur.Parent))
		}
		if parent.Canonical {
			return parent.ID, nil
		}
		cur = parent
	}
	return "", fmt.Errorf("artifact %s does not descend from a canonical snapshot", short(m.ID))
}

// Landings lists landings of a project, newest first.
// Corrupt records are reported with their IDs alongside the valid results.
func (s *Store) Landings(ctx context.Context, projectID string) ([]Landing, error) {
	ops, err := s.ledger.Operations(ctx, landKind, "")
	if err != nil {
		return nil, err
	}
	var out []Landing
	var failures []error
	for _, op := range ops {
		var l Landing
		if err := json.Unmarshal(op.Data, &l); err != nil {
			failures = append(failures, fmt.Errorf("read landing %s: %w", op.ID, err))
			continue
		}
		if l.Project == projectID {
			l.State = op.State
			out = append(out, l)
		}
	}
	return out, errors.Join(failures...)
}
