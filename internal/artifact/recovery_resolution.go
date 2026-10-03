package artifact

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

const recoveryResolutionKind = "recovery-resolution"
const recoveryWinnerKind = "recovery-landing-result"

type acceptedRecoveryResolution struct {
	Ref      attempt.RecoveryResolutionRef `json:"ref"`
	Artifact string                        `json:"artifact"`
	Source   *Source                       `json:"source,omitempty"`
	Manual   bool                          `json:"manual"`
}

// Install once at composition time. Only the exact resolution consumer invokes
// this owner/maintenance/driver boundary, never an ordinary landing or exec sink.
func (s *Store) SetRecoveryResolutionDriver(drive func(context.Context, string, func(context.Context, ledger.Lease) error) error) {
	s.recoveryResolutionDriver = drive
}

func RecoveryResolutionID(ref attempt.RecoveryResolutionRef, artifactID string, source *Source) string {
	raw, _ := json.Marshal(struct {
		Ref      attempt.RecoveryResolutionRef
		Artifact string
		Source   *Source
	}{ref, artifactID, source})
	sum := sha256.Sum256(raw)
	return "recovery-resolution:" + ref.Root + ":" + hex.EncodeToString(sum[:])
}

func recoveryLandingTx(tx ledger.Reader, id string) (Landing, error) {
	var raw, state string
	if err := tx.QueryRow(`SELECT data,state FROM operations WHERE kind=? AND id=?`, landKind, id).Scan(&raw, &state); err != nil {
		return Landing{}, err
	}
	var land Landing
	if json.Unmarshal([]byte(raw), &land) != nil || land.ID != id {
		return land, errors.New("recovery landing identity is invalid")
	}
	land.State = state
	return land, nil
}

func recoveryConflictTx(tx *ledger.Tx, r attempt.WorkspaceRecovery, ref attempt.RecoveryResolutionRef) (Landing, error) {
	if ref.Episode != r.ID || ref.HeadVersion != r.FrozenHeadVersion || ref.Root != attempt.RecoveryLandingID(r) || ref.Conflict == "" || ref.Marked == "" || ref.Canonical == "" {
		return Landing{}, ErrNotBlocked
	}
	land, err := recoveryLandingTx(tx, ref.Conflict)
	if err != nil {
		return land, err
	}
	if land.Recovery == nil || land.Recovery.Episode != r.ID || land.Recovery.HeadVersion != ref.HeadVersion || land.Target != r.Target || land.Project != r.Project || land.Conflict != ref.Marked || land.Now != ref.Canonical || land.State != LandMergeConflicted && land.State != LandApplyConflicted {
		return land, ErrNotBlocked
	}
	var raw []byte
	if err := tx.QueryRow(`SELECT data FROM bindings WHERE kind=? AND id=?`, pendingKind, r.Project+"/"+r.Head.Artifact).Scan(&raw); err != nil {
		return land, err
	}
	var pending Pending
	if json.Unmarshal(raw, &pending) != nil || pending.Recovery == nil || pending.Recovery.Episode != r.ID || pending.Recovery.HeadVersion != ref.HeadVersion || pending.Blocked == nil || pending.Blocked.Landing != ref.Conflict {
		return land, ErrNotBlocked
	}
	var winner string
	if err := tx.QueryRow(`SELECT data FROM bindings WHERE kind=? AND id=?`, recoveryWinnerKind, ref.Root).Scan(&winner); err == nil {
		return land, ErrNotBlocked
	} else if !errors.Is(err, sql.ErrNoRows) {
		return land, err
	}
	return land, nil
}

// LandRecoveryResolutionOnce is the only recovered plan sink. It acquires the
// owner-controlled episode driver once, then calls the already-authorized
// executor directly; it never recurses through a public driver entrance.
func (s *Store) LandRecoveryResolutionOnce(ctx context.Context, ref attempt.RecoveryResolutionRef, artifactID string, source Source) (Landing, error) {
	if s.recoveryResolutionDriver == nil || source.Execution == nil || source.AttemptID == "" {
		return Landing{}, errors.New("recovery resolution requires its owner driver and resolver execution")
	}
	var result Landing
	err := s.recoveryResolutionDriver(ctx, ref.Episode, func(ctx context.Context, driver ledger.Lease) error {
		var err error
		result, err = s.executeRecoveryResolution(ctx, ref, artifactID, &source, false, driver)
		return err
	})
	return result, err
}

func (s *Store) executeRecoveryResolution(ctx context.Context, ref attempt.RecoveryResolutionRef, artifactID string, source *Source, manual bool, driver ledger.Lease) (Landing, error) {
	r, err := attempt.New(s.ledger).WorkspaceRecovery(ctx, ref.Episode)
	if err != nil {
		return Landing{}, err
	}
	p, found, err := s.projects.Get(ctx, r.Project)
	if err != nil {
		return Landing{}, err
	}
	if !found {
		return Landing{}, project.ErrUnknown
	}
	id := RecoveryResolutionID(ref, artifactID, source)
	link := RecoveryLink{Episode: ref.Episode, HeadVersion: ref.HeadVersion, Resolution: &ref}
	ctx = context.WithValue(ctx, recoveryLandingKey{}, &recoveryLandingPermit{link: link, driver: driver, lifetime: attempt.RecoveryDriverLifetime(ctx)})
	_, existing, err := s.ledger.Operation(ctx, id)
	if err != nil {
		return Landing{}, err
	}
	if !existing {
		err = s.ledger.Update(ctx, func(tx *ledger.Tx) error {
			current, err := attempt.CheckRecoveryLandingTx(tx, r.ID, ref.HeadVersion, driver, nil, true)
			if err != nil {
				return err
			}
			if _, err := recoveryConflictTx(tx, current, ref); err != nil {
				return err
			}
			content, err := s.RecoveryOutputTx(tx, r.Project, ref.Marked, artifactID)
			if err != nil {
				return err
			}
			if !manual {
				if err := checkResolutionSourceTx(tx, current.Project, artifactID, source); err != nil {
					return err
				}
				if err := attempt.RetainRecoveryResolverTx(tx, r.ID, ref.HeadVersion, driver, attempt.RecoveryResolver{Landing: id, Attempt: source.AttemptID, Execution: *source.Execution, Artifact: artifactID, RecoveryContent: content}); err != nil {
					return err
				}
			}
			return tx.PutBinding(recoveryResolutionKind, id, acceptedRecoveryResolution{Ref: ref, Artifact: artifactID, Source: source, Manual: manual})
		})
		if err != nil {
			return Landing{}, err
		}
	}
	var sources []Source
	if source != nil {
		sources = []Source{*source}
	}
	land, err := s.LandOnce(ctx, id, p, artifactID, "workspace recovery resolution", sources...)
	var conflict Conflict
	if errors.As(err, &conflict) {
		if queueErr := s.queueRecoveryConflict(ctx, p, land, conflict); queueErr != nil {
			return land, errors.Join(err, queueErr)
		}
	}
	return land, err
}

func checkResolutionSourceTx(tx ledger.Reader, projectID, artifactID string, source *Source) error {
	if source == nil || source.Execution == nil || source.AttemptID == "" {
		return task.ErrExecutionStopped
	}
	r, err := attempt.GetTx(tx, source.AttemptID)
	if err != nil {
		return err
	}
	if r.Project != projectID || r.Execution == nil || *r.Execution != *source.Execution || !r.State.Terminal() || r.Result == nil || r.Result.Artifact != artifactID || r.Result.CaptureError != "" {
		return errors.New("resolution artifact differs from its accepted producer")
	}
	return task.CheckExecutionTx(tx, source.Execution)
}

func acceptedResolutionTx(tx ledger.Reader, land Landing) (acceptedRecoveryResolution, error) {
	var raw []byte
	if err := tx.QueryRow(`SELECT data FROM bindings WHERE kind=? AND id=?`, recoveryResolutionKind, land.ID).Scan(&raw); err != nil {
		return acceptedRecoveryResolution{}, err
	}
	var accepted acceptedRecoveryResolution
	if json.Unmarshal(raw, &accepted) != nil || land.Recovery == nil || land.Recovery.Resolution == nil || accepted.Ref != *land.Recovery.Resolution || accepted.Artifact != land.Artifact || !sameSource(accepted.Source, land.Source) {
		return accepted, errors.New("resolution acceptance differs from its exact landing")
	}
	return accepted, nil
}

func sameSource(a, b *Source) bool {
	ra, _ := json.Marshal(a)
	rb, _ := json.Marshal(b)
	return string(ra) == string(rb)
}

func (s *Store) resolveRecoveryByHand(ctx context.Context, p project.Project, stuck Stuck, edits []Edit) (Landing, error) {
	if s.recoveryResolutionDriver == nil || stuck.Recovery == nil || stuck.Resolution == nil {
		return Landing{}, ErrNotBlocked
	}
	if metadataOnly(p) {
		return Landing{}, ErrSealedByHand
	}
	ref := *stuck.Resolution
	var result Landing
	err := s.recoveryResolutionDriver(ctx, ref.Episode, func(ctx context.Context, driver ledger.Lease) error {
		if err := s.ledger.Update(ctx, func(tx *ledger.Tx) error {
			r, err := attempt.CheckRecoveryLandingTx(tx, ref.Episode, ref.HeadVersion, driver, nil, true)
			if err != nil {
				return err
			}
			_, err = recoveryConflictTx(tx, r, ref)
			return err
		}); err != nil {
			return err
		}
		wanted, err := resolvedFiles(stuck, edits)
		if err != nil {
			return err
		}
		if err := s.BringHome(ctx, p, ref.Marked); err != nil {
			return err
		}
		ws, err := s.Materialize(ctx, project.Request{Project: p.ID, Isolated: true, Base: ref.Marked, Owner: "resolve-" + ref.Episode})
		if err != nil {
			return err
		}
		defer s.Discard(context.WithoutCancel(ctx), ws)
		if err := writeResolution(ws.Path, wanted); err != nil {
			return err
		}
		m, _, err := s.snapshotRecoveryResolution(ctx, p, ws, ref.Marked, "workspace recovery", recordGuard{check: func(tx *ledger.Tx) error {
			current, err := attempt.CheckRecoveryLandingTx(tx, ref.Episode, ref.HeadVersion, driver, nil, true)
			if err != nil {
				return err
			}
			_, err = recoveryConflictTx(tx, current, ref)
			return err
		}})
		if err != nil {
			return err
		}
		result, err = s.executeRecoveryResolution(ctx, ref, m.ID, nil, true, driver)
		return err
	})
	return result, err
}

func (s *Store) queueRecoveryConflict(ctx context.Context, p project.Project, land Landing, conflict Conflict) error {
	return s.ledger.Update(ctx, func(tx *ledger.Tx) error {
		r, err := checkRecoveryLandingPermitTx(ctx, tx, p, &land, nil, false)
		if err != nil {
			return err
		}
		publish, err := recoveryConflictPublicationTx(tx, r, land)
		if err != nil || !publish {
			return err
		}
		ref := &attempt.RecoveryResolutionRef{Episode: r.ID, HeadVersion: r.FrozenHeadVersion, Root: attempt.RecoveryLandingID(r), Conflict: land.ID, Marked: land.Conflict, Canonical: land.Now}
		item := Pending{Project: p.ID, Artifact: r.Head.Artifact, By: "workspace recovery", At: s.now().UTC(), Recovery: land.Recovery, Resolution: ref, Blocked: &Blocked{Landing: land.ID, State: land.State, Canonical: land.Now, Marked: land.Conflict, Paths: conflict.Paths, At: s.now().UTC(), Reason: conflict.Reason}}
		return tx.PutBinding(pendingKind, p.ID+"/"+r.Head.Artifact, item)
	})
}

// A conflict may publish only over its exact predecessor. A terminal retry
// acknowledges the old outcome without rewinding a successor or a winner.
func recoveryConflictPublicationTx(tx *ledger.Tx, r attempt.WorkspaceRecovery, land Landing) (bool, error) {
	stored, err := recoveryLandingTx(tx, land.ID)
	if err != nil {
		return false, err
	}
	if stored.State != LandMergeConflicted && stored.State != LandApplyConflicted || stored.Project != r.Project || stored.Target != r.Target || stored.Now != land.Now || stored.Conflict != land.Conflict || !sameRecoveryLink(stored.Recovery, land.Recovery) {
		return false, ErrNotBlocked
	}
	if _, err := recoveryWinnerTx(tx, r); err == nil {
		return false, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	var raw []byte
	err = tx.QueryRow(`SELECT data FROM bindings WHERE kind=? AND id=?`, pendingKind, r.Project+"/"+r.Head.Artifact).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return land.Recovery.Resolution == nil, nil
	}
	if err != nil {
		return false, err
	}
	var item Pending
	if json.Unmarshal(raw, &item) != nil || item.Project != r.Project || item.Artifact != r.Head.Artifact || item.Recovery == nil || item.Recovery.Episode != r.ID || item.Recovery.HeadVersion != r.FrozenHeadVersion || item.Resolution == nil || item.Blocked == nil {
		return false, ErrNotBlocked
	}
	current, err := recoveryLandingTx(tx, item.Blocked.Landing)
	if err != nil {
		return false, err
	}
	expected := attempt.RecoveryResolutionRef{Episode: r.ID, HeadVersion: r.FrozenHeadVersion, Root: attempt.RecoveryLandingID(r), Conflict: current.ID, Marked: current.Conflict, Canonical: current.Now}
	if current.State != LandMergeConflicted && current.State != LandApplyConflicted || current.Project != r.Project || current.Target != r.Target || !sameRecoveryLink(item.Recovery, current.Recovery) || *item.Resolution != expected || item.Blocked.State != current.State || item.Blocked.Marked != current.Conflict || item.Blocked.Canonical != current.Now {
		return false, ErrNotBlocked
	}
	if current.ID == land.ID || land.Recovery.Resolution == nil {
		return false, nil
	}
	return *land.Recovery.Resolution == *item.Resolution, nil
}

func recordRecoveryWinnerTx(tx *ledger.Tx, r attempt.WorkspaceRecovery, land Landing) error {
	if land.Recovery == nil || land.Committed == nil {
		return errors.New("recovery winner is not committed")
	}
	return tx.PutBinding(recoveryWinnerKind, attempt.RecoveryLandingID(r), land.ID)
}

func recoveryWinnerTx(tx ledger.Reader, r attempt.WorkspaceRecovery) (Landing, error) {
	var raw []byte
	if err := tx.QueryRow(`SELECT data FROM bindings WHERE kind=? AND id=?`, recoveryWinnerKind, attempt.RecoveryLandingID(r)).Scan(&raw); err != nil {
		return Landing{}, err
	}
	var id string
	if json.Unmarshal(raw, &id) != nil || id == "" {
		return Landing{}, errors.New("recovery winner identity is invalid")
	}
	land, err := recoveryLandingTx(tx, id)
	if err != nil {
		return land, err
	}
	if land.State != LandCommitted || land.Recovery == nil || land.Recovery.Episode != r.ID || land.Recovery.HeadVersion != r.FrozenHeadVersion || land.Project != r.Project || land.Target != r.Target || land.Committed == nil {
		return land, fmt.Errorf("recovery result is not its exact committed landing")
	}
	return land, nil
}

func checkResolutionLandingTx(tx *ledger.Tx, r attempt.WorkspaceRecovery, land Landing, newAdmission bool) error {
	accepted, err := acceptedResolutionTx(tx, land)
	if err != nil {
		return err
	}
	ref := accepted.Ref
	if ref.Episode != r.ID || ref.HeadVersion != r.FrozenHeadVersion || ref.Root != attempt.RecoveryLandingID(r) || land.ID != RecoveryResolutionID(ref, land.Artifact, land.Source) || land.Base != ref.Canonical || accepted.Manual != (accepted.Source == nil) {
		return ErrNotBlocked
	}
	previous, err := recoveryLandingTx(tx, ref.Conflict)
	if err != nil {
		return err
	}
	if previous.Recovery == nil || previous.Recovery.Episode != r.ID || previous.Recovery.HeadVersion != ref.HeadVersion || previous.Project != r.Project || previous.Target != r.Target || previous.Conflict != ref.Marked || previous.Now != ref.Canonical {
		return ErrNotBlocked
	}
	m, err := artifactTx(tx, land.Artifact)
	if err != nil {
		return err
	}
	if m.Project != r.Project || m.Parent != ref.Marked {
		return errors.New("resolved artifact does not descend from its exact marked conflict")
	}
	if newAdmission {
		if err := checkResolutionAncestorAuthorizationTx(tx, previous); err != nil {
			return err
		}
		if _, err := recoveryConflictTx(tx, r, ref); err != nil {
			return err
		}
		if !accepted.Manual {
			return checkResolutionSourceTx(tx, r.Project, land.Artifact, land.Source)
		}
	}
	return nil
}

// ResumeRecoveryResolution is the episode driver's independent consumer of
// an already admitted resolution WAL. It does not authorize a new source or
// reacquire the owner callback/episode lease recursively.
func (s *Store) ResumeRecoveryResolution(ctx context.Context, id string, driver ledger.Lease) (bool, error) {
	r, err := attempt.New(s.ledger).WorkspaceRecovery(ctx, id)
	if err != nil {
		return false, err
	}
	raw, err := s.ledger.Bindings(ctx, recoveryResolutionKind)
	if err != nil {
		return false, err
	}
	var candidate *acceptedRecoveryResolution
	for key, data := range raw {
		var accepted acceptedRecoveryResolution
		if json.Unmarshal(data, &accepted) != nil {
			return false, errors.New("unreadable recovery resolution")
		}
		if accepted.Ref.Episode != id {
			continue
		}
		if key != RecoveryResolutionID(accepted.Ref, accepted.Artifact, accepted.Source) || accepted.Ref.HeadVersion != r.FrozenHeadVersion || accepted.Ref.Root != attempt.RecoveryLandingID(r) {
			return false, ErrNotBlocked
		}
		op, found, err := s.ledger.Operation(ctx, key)
		if err != nil {
			return false, err
		}
		if !found || op.State != LandApplying && op.State != LandRecoveryPending {
			continue
		}
		if candidate != nil {
			return false, errors.New("workspace recovery has conflicting admitted resolution WALs")
		}
		copy := accepted
		candidate = &copy
	}
	if candidate == nil {
		return false, nil
	}
	_, err = s.executeRecoveryResolution(ctx, candidate.Ref, candidate.Artifact, candidate.Source, candidate.Manual, driver)
	return true, err
}

func checkResolutionAncestorAuthorizationTx(tx ledger.Reader, land Landing) error {
	seen := map[string]bool{}
	for land.Recovery != nil && land.Recovery.Resolution != nil {
		if seen[land.ID] || len(seen) > 64 {
			return errors.New("resolution lineage is cyclic or too deep")
		}
		seen[land.ID] = true
		if land.Source != nil {
			if err := checkResolutionSourceTx(tx, land.Project, land.Artifact, land.Source); err != nil {
				return err
			}
		}
		previous, err := recoveryLandingTx(tx, land.Recovery.Resolution.Conflict)
		if err != nil {
			return err
		}
		if previous.Recovery == nil || previous.Recovery.Episode != land.Recovery.Episode || previous.Recovery.HeadVersion != land.Recovery.HeadVersion || previous.Target != land.Target || previous.Project != land.Project {
			return ErrNotBlocked
		}
		land = previous
	}
	return nil
}

func (s *Store) currentRecoveryConflict(ctx context.Context, r attempt.WorkspaceRecovery) (Landing, bool, error) {
	var item Pending
	found, err := s.ledger.GetBinding(ctx, pendingKind, r.Project+"/"+r.Head.Artifact, &item)
	if err != nil || !found {
		return Landing{}, false, err
	}
	if item.Recovery == nil || item.Recovery.Episode != r.ID || item.Recovery.HeadVersion != r.FrozenHeadVersion || item.Blocked == nil {
		return Landing{}, false, ErrNotBlocked
	}
	var land Landing
	err = s.ledger.Read(ctx, func(tx *ledger.ReadTx) error {
		var err error
		land, err = recoveryLandingTx(tx, item.Blocked.Landing)
		return err
	})
	if err != nil {
		return land, false, err
	}
	if land.Recovery == nil || land.Recovery.Episode != r.ID || land.Recovery.HeadVersion != r.FrozenHeadVersion {
		return land, false, ErrNotBlocked
	}
	return land, true, nil
}
