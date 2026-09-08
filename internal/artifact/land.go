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
//	applying → apply-conflicted     (a path changed under the WAL)
//	merged   → commit-conflicted    (the canonical name moved)
//
// The canonical lock is held from locked to the end. Every path written is
// journaled as an effect — started before, confirmed after — under the WAL
// round, so a landing cut off mid-apply is recovered per path rather than
// guessed at.
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
	Source         *Source       `json:"source,omitempty"`
	ID             string        `json:"id"`
	Project        string        `json:"project"`
	Artifact       string        `json:"artifact"`
	Base           string        `json:"base,omitempty"`
	Now            string        `json:"now,omitempty"`
	Merged         string        `json:"merged,omitempty"`
	By             string        `json:"by,omitempty"`
	State          string        `json:"state"`
	Paths          []string      `json:"paths,omitempty"`
	Round          int           `json:"round"`
	Error          string        `json:"error,omitempty"`
	Lease          *ledger.Lease `json:"lease,omitempty"`
	StartedAt      time.Time     `json:"started_at"`
	EndedAt        time.Time     `json:"ended_at,omitempty"`
	Recoverable    bool          `json:"recoverable,omitempty"`
}

const (
	LandProposed        = "proposed"
	LandLocked          = "locked"
	LandMerged          = "merged"
	LandApplying        = "applying"
	LandCommitted       = "committed"
	LandMergeConflicted = "merge-conflicted"
	LandApplyConflicted = "apply-conflicted"
	LandCommitConflict  = "commit-conflicted"
	landKind            = "landing"
	landTTL             = 5 * time.Minute
)

// Conflict is a landing that stopped at a conflict, with the paths.
type Conflict struct {
	State string
	Paths []string
}

func (c Conflict) Error() string {
	return fmt.Sprintf("landing %s: %s", c.State, strings.Join(c.Paths, ", "))
}

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

func (s *Store) land(ctx context.Context, p project.Project, artifactID, by string, held *ledger.Lease, source *Source, resume *Landing) (Landing, error) {
	borrowedHolder := ""
	if held != nil {
		if held.Key != "canonical:"+p.ID {
			return Landing{}, fmt.Errorf("landing lease %s does not own project %s", held.Key, p.ID)
		}
		if err := s.ledger.CheckAny(ctx, *held); err != nil {
			return Landing{}, err
		}
		borrowedHolder = held.Holder
	}
	if err := s.ledger.Update(ctx, func(tx *ledger.Tx) error { return attempt.CheckWriterTx(tx, p.Home.Node, p.Home.Path, borrowedHolder) }); err != nil {
		return Landing{}, err
	}
	if source != nil {
		if source.Execution == nil {
			return Landing{}, errors.New("landing source requires execution authorization")
		}
		if s.executions != nil {
			scope, err := s.executions.BeginAccepted(ctx, execution.Key{TaskID: source.Execution.TaskID, InstanceID: "land/" + artifactID, AttemptID: source.AttemptID}, source.Execution)
			if err != nil {
				return Landing{}, err
			}
			defer scope.Finish(nil)
			ctx = scope.Context()
		}
		if err := s.ledger.Update(ctx, func(tx *ledger.Tx) error { return task.CheckExecutionTx(tx, source.Execution) }); err != nil {
			return Landing{}, err
		}
	}
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
	if resume == nil || resume.State == "" {
		if _, err := s.ledger.Begin(ctx, land.ID, landKind, LandProposed, by, land); err != nil {
			return Landing{}, err
		}
	}
	var lease ledger.Lease
	if held != nil {
		if held.Key != "canonical:"+p.ID {
			return land, fmt.Errorf("landing %s: the lease offered is for %s, not the project's canonical", land.ID, held.Key)
		}
		lease = *held
	} else {
		acquired, err := s.ledger.AcquireIn(ctx, s.homeRegion(ctx, p), "canonical:"+p.ID, landingHolder(ctx, land.ID), landTTL)
		if err != nil {
			if !land.Recoverable {
				s.failed(ctx, &land, LandProposed, LandMergeConflicted, err.Error(), nil)
			}
			return land, err
		}
		lease = acquired
		// The lock falls to its TTL when the release fails; nothing else
		// can be done about it here, and the landing's own result stands.
		defer func() { _ = s.ledger.ReleaseAny(context.WithoutCancel(ctx), lease) }()
		defer trackLandingLease(ctx, lease)()
	}
	land.Lease = &lease
	if err := s.move(ctx, &land, LandProposed, LandLocked, nil); err != nil {
		return land, err
	}

	// Under the lock: what the canonical workspace holds right now.
	now, _, err := s.SnapshotCanonical(ctx, p, s.canonicalRef(ctx, p.ID), land.ID, "before landing "+short(artifactID))
	if err != nil {
		if !land.Recoverable {
			s.failed(ctx, &land, LandLocked, LandMergeConflicted, "snapshot: "+err.Error(), nil)
		}
		return land, err
	}
	land.Now = now.ID
	repo, err := s.Repo(ctx, p.ID)
	if err != nil {
		return land, err
	}
	var merged string
	var conflicts []string
	if metadataOnly(p) {
		merged, conflicts, err = s.mergeOnNode(ctx, p, base, now.ID, artifactID)
	} else {
		merged, conflicts, err = repo.Merge(ctx, base, now.ID, artifactID, "land "+short(artifactID)+" into "+p.ID)
	}
	if err != nil {
		if !land.Recoverable {
			s.failed(ctx, &land, LandLocked, LandMergeConflicted, err.Error(), nil)
		}
		return land, err
	}
	if len(conflicts) > 0 {
		s.failed(ctx, &land, LandLocked, LandMergeConflicted, "conflicts", conflicts)
		return land, Conflict{State: LandMergeConflicted, Paths: conflicts}
	}
	land.Merged = merged
	var paths []string
	if metadataOnly(p) {
		paths, err = s.changedOnNode(ctx, p, now.ID, merged)
	} else {
		paths, err = repo.Changed(ctx, now.ID, merged)
	}
	if err != nil {
		return land, err
	}
	land.Paths = paths
	if err := s.move(ctx, &land, LandLocked, LandMerged, nil); err != nil {
		return land, err
	}
	if len(paths) == 0 {
		// Nothing to write: the canonical already has it all.
		if err := s.move(ctx, &land, LandMerged, LandCommitted, nil); err != nil {
			return land, err
		}
		return land, nil
	}
	land.Round++
	if err := s.move(ctx, &land, LandMerged, LandApplying, nil); err != nil {
		return land, err
	}
	// Applying is already admitted under the source epoch. Finish this WAL
	// operation even if a later stop revokes permission for new work.
	applyCtx, finishApply := landingApplyContext(ctx)
	defer finishApply()
	ctx = applyCtx
	journal := s.ledger.Journal()
	for _, path := range paths {
		if _, err := journal.Started(ledger.EffectID{Operation: land.ID, Kind: "land-path", InstanceKey: fmt.Sprintf("%d/%s", land.Round, path)}, "", nil); err != nil {
			return land, err
		}
	}
	if p.Home.Node == "" {
		_, err = repo.Apply(ctx, now.ID, merged, p.Home.Path)
	} else {
		err = s.applyOnNode(ctx, p, now.ID, merged, repo)
	}
	if err != nil {
		if land.Recoverable {
			return land, err
		}
		s.failed(ctx, &land, LandApplying, LandApplyConflicted, err.Error(), paths)
		return land, Conflict{State: LandApplyConflicted, Paths: paths}
	}
	for _, path := range paths {
		if _, err := journal.Confirmed(ledger.EffectID{Operation: land.ID, Kind: "land-path", InstanceKey: fmt.Sprintf("%d/%s", land.Round, path)}, nil); err != nil {
			return land, err
		}
	}
	// Committed: the canonical name moves to the merged snapshot in the
	// same transaction as the state, fenced on the lock.
	current, _, _ := s.ledger.Name(ctx, CanonicalRef(p.ID))
	land.State = LandCommitted
	land.EndedAt = s.now().UTC()
	_, err = s.ledger.Transition(ctx, land.ID, LandApplying, LandCommitted, by, landingFence(ctx, []ledger.Lease{lease}), map[string]any{"paths": paths},
		func(tx *ledger.Tx, op *ledger.Operation) error {
			if _, err := tx.CompareAndSetName(CanonicalRef(p.ID), current.Version, merged); err != nil {
				return err
			}
			return tx.SetData(op, land)
		})
	if err != nil {
		if errors.Is(err, ledger.ErrConflict) {
			s.failed(ctx, &land, LandApplying, LandCommitConflict, err.Error(), paths)
			return land, Conflict{State: LandCommitConflict, Paths: paths}
		}
		return land, err
	}
	if _, err := s.receipt(ctx, p, Manifest{ID: merged, Project: p.ID, Parent: now.ID, Label: p.Level, By: land.ID, Message: "landed " + short(artifactID), Canonical: true}); err != nil {
		return land, err
	}
	return land, nil
}

// mergeOnNode three-way merges at the home node of a sealed project, which
// needs the node's git to know merge-tree --write-tree (2.38+).
func (s *Store) mergeOnNode(ctx context.Context, p project.Project, base, ours, theirs string) (string, []string, error) {
	version, _, state, err := s.nodes.Git(ctx, p.Home.Node)
	if err != nil {
		return "", nil, err
	}
	if ours == base {
		return theirs, nil, nil
	}
	if theirs == base {
		return ours, nil, nil
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
			return "", conflict.Paths, nil
		}
		return "", nil, fmt.Errorf("merge on %s: %w", p.Home.Node, err)
	}
	return result.Commit, nil, nil
}

func (s *Store) changedOnNode(ctx context.Context, p project.Project, from, to string) ([]string, error) {
	_, _, state, err := s.nodes.Git(ctx, p.Home.Node)
	if err != nil {
		return nil, err
	}
	result, err := s.nodes.Artifact(ctx, p.Home.Node, ops.Request{Op: ops.Changed, Repo: nodeBare(state, p.ID), From: from, Commit: to})
	return result.Paths, err
}

// applyOnNode writes the merged tree into a canonical workspace that lives
// on a node: the merged commit is pushed and applied there with the same
// two-tree merge the hub uses.
func (s *Store) applyOnNode(ctx context.Context, p project.Project, from, merged string, hub *Repo) error {
	_, _, state, err := s.nodes.Git(ctx, p.Home.Node)
	if err != nil {
		return err
	}
	bare := nodeBare(state, p.ID)
	if !metadataOnly(p) && !s.nodeHas(ctx, p.Home.Node, bare, merged) {
		if err := s.push(ctx, p.Home.Node, bare, hub, merged, []string{from}); err != nil {
			return err
		}
	}
	_, err = s.nodes.Artifact(ctx, p.Home.Node, ops.Request{Op: ops.Apply, Repo: bare, From: from, Commit: merged, WorkTree: p.Home.Path})
	return err
}

func (s *Store) move(ctx context.Context, land *Landing, from, to string, effects any) error {
	land.State = to
	var fencings []ledger.Lease
	if land.Lease != nil {
		fencings = []ledger.Lease{*land.Lease}
	}
	_, err := s.ledger.Transition(ctx, land.ID, from, to, land.By, landingFence(ctx, fencings), effects,
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

func (s *Store) fail(ctx context.Context, land *Landing, from, to, cause string, paths []string) error {
	land.Error = cause
	if paths != nil {
		land.Paths = paths
	}
	land.EndedAt = s.now().UTC()
	return s.move(context.WithoutCancel(ctx), land, from, to, map[string]any{"paths": paths})
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
func (s *Store) Landings(ctx context.Context, projectID string) ([]Landing, error) {
	ops, err := s.ledger.Operations(ctx, landKind, "")
	if err != nil {
		return nil, err
	}
	var out []Landing
	for _, op := range ops {
		var l Landing
		if err := json.Unmarshal(op.Data, &l); err != nil {
			continue
		}
		if l.Project == projectID {
			l.State = op.State
			out = append(out, l)
		}
	}
	return out, nil
}
