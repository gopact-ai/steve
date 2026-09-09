package attempt

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/checkpoint"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/plugins"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

const relocationKind = "attempt-relocation"
const retiredRelocationKind = "attempt-relocation-retired"

func PreparingRelocation(r Record) bool {
	return r.Kind == KindChat && r.Recovery != nil && r.Execution != nil && (r.State == Leased || r.State == Prepared)
}

// RecoverRelocationPreparation adopts the same approved preparation and exact
// lease tuples. Node dispatch requires Running, so this phase cannot have sent
// task input. Any native open is retried with its same idempotent command and
// saved configuration; this neither proves a process stopped nor replaces it.
func (s *Service) RecoverRelocationPreparation(ctx context.Context, id string) (Record, error) {
	current, err := s.Get(ctx, id)
	if err != nil {
		return Record{}, err
	}
	var next Record
	_, err = s.l.Transition(ctx, id, string(current.State), string(current.State), "relocation-preparation", nil, nil, func(tx *ledger.Tx, op *ledger.Operation) error {
		if err := json.Unmarshal(op.Data, &next); err != nil {
			return err
		}
		if !PreparingRelocation(next) {
			return errors.New("execution is no longer an approved preparation")
		}
		if err := task.CheckExecutionTx(tx, next.Execution); err != nil {
			return err
		}
		var raw string
		if err := tx.QueryRow(`SELECT data FROM bindings WHERE kind = ? AND id = ?`, relocationKind, next.Recovery.PlanID).Scan(&raw); err != nil {
			return err
		}
		var plan RelocationIntent
		if json.Unmarshal([]byte(raw), &plan) != nil || plan.ID != plan.Fingerprint() || plan.Target.ID != next.ID || plan.SourceID != next.Recovery.AttemptID {
			return errors.New("preparation differs from its approved plan")
		}
		leases, err := tx.RenewRetained(next.Leases, s.TTL)
		if err != nil {
			return err
		}
		next.Leases, next.Unsettled, next.Error, next.Revision = leases, false, "", op.Revision+1
		return tx.SetData(op, next)
	})
	return next, err
}

// Relocatable includes an unstarted replacement whose preparation crashed.
// Such a record still needs explicit stopping proof/approval before another
// execution is created; phase alone is never physical stop evidence.
func Relocatable(r Record) bool {
	if r.Kind != KindChat || r.Execution == nil {
		return false
	}
	if r.State == Running {
		return true
	}
	if r.Recovery == nil || r.Session != "" || r.SessionSettled == nil || !*r.SessionSettled {
		return false
	}
	switch r.State {
	case Leased, Prepared, Expired, Failed:
		return true
	}
	return false
}

type RelocationIntent struct {
	Plugins          *plugins.Relocation         `json:"plugins,omitempty"`
	ID               string                      `json:"id"`
	SourceID         string                      `json:"source_id"`
	SourceRevision   int64                       `json:"source_revision"`
	TaskEpoch        uint64                      `json:"task_epoch"`
	Checkpoint       string                      `json:"checkpoint"`
	Target           Spec                        `json:"target"`
	TargetConfigHash string                      `json:"target_config_hash"`
	Prompt           string                      `json:"prompt"`
	InputDigest      string                      `json:"input_digest"`
	UnknownActions   []checkpoint.ExternalAction `json:"unknown_actions,omitempty"`
	CreatedAt        time.Time                   `json:"created_at"`
	Owner            string                      `json:"owner"`
}

func (p RelocationIntent) Fingerprint() string {
	p.ID = ""
	raw, _ := json.Marshal(p)
	sum := sha256.Sum256(raw)
	return "relocate-" + hex.EncodeToString(sum[:])
}

// RelocationApproval is scoped to one immutable plan. A platform question's
// explicit choice may attest stopping and reviewing uncontrolled CLI effects;
// arbitrary free text or a bare node timeout cannot set these fields.
type RelocationApproval struct {
	PlanID           string                        `json:"plan_id"`
	Actor            string                        `json:"actor"`
	ChoiceID         string                        `json:"choice_id"`
	StoppedConfirmed bool                          `json:"stopped_confirmed"`
	EffectsReviewed  bool                          `json:"effects_reviewed"`
	ActionResults    []checkpoint.ActionResolution `json:"action_results,omitempty"`
	Node             *RetainedEvidence             `json:"node,omitempty"`
}

func (s *Service) RecordRelocation(ctx context.Context, p RelocationIntent) (RelocationIntent, error) {
	if p.ID == "" {
		p.ID = p.Fingerprint()
	}
	if p.ID != p.Fingerprint() || p.Owner == "" || p.Prompt == "" || p.InputDigest == "" || p.Checkpoint == "" || p.Target.ID == "" || p.Target.ID == p.SourceID {
		return RelocationIntent{}, errors.New("invalid relocation plan")
	}
	err := s.l.Update(ctx, func(tx *ledger.Tx) error {
		ops, err := tx.Operations(kind, "")
		if err != nil {
			return err
		}
		for _, op := range ops {
			if op.ID == p.SourceID {
				r, err := decode(op)
				if err != nil {
					return err
				}
				if r.Revision != p.SourceRevision || r.Execution == nil || r.Execution.Epoch != p.TaskEpoch {
					return ledger.ErrConflict
				}
				if err := task.CheckExecutionTx(tx, r.Execution); err != nil {
					return err
				}
				return tx.PutBinding(relocationKind, p.ID, p)
			}
		}
		return errors.New("relocation source missing")
	})
	return p, err
}

func (s *Service) Relocation(ctx context.Context, id string) (RelocationIntent, error) {
	var p RelocationIntent
	ok, err := s.l.GetBinding(ctx, relocationKind, id, &p)
	if err != nil {
		return p, err
	}
	if !ok || p.ID != id || p.Fingerprint() != id {
		return RelocationIntent{}, errors.New("relocation plan is missing or changed")
	}
	return p, nil
}

func (s *Service) RelocationsFor(ctx context.Context, source string) ([]RelocationIntent, error) {
	raw, err := s.l.Bindings(ctx, relocationKind)
	if err != nil {
		return nil, err
	}
	retired, err := s.l.Bindings(ctx, retiredRelocationKind)
	if err != nil {
		return nil, err
	}
	var result []RelocationIntent
	for id, data := range raw {
		if _, ok := retired[id]; ok {
			continue
		}
		var p RelocationIntent
		if json.Unmarshal(data, &p) != nil || p.ID != id || p.Fingerprint() != id {
			return nil, errors.New("relocation index is corrupt")
		}
		if source == "" || p.SourceID == source {
			result = append(result, p)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.After(result[j].CreatedAt) })
	return result, nil
}

func (s *Service) InvalidateRelocation(ctx context.Context, id, reason string) error {
	if reason == "" {
		return errors.New("relocation invalidation reason is required")
	}
	p, err := s.Relocation(ctx, id)
	if err != nil {
		return err
	}
	return s.l.Update(ctx, func(tx *ledger.Tx) error {
		var found string
		err := tx.QueryRow(`SELECT id FROM operations WHERE id = ?`, p.Target.ID).Scan(&found)
		if err == nil {
			return errors.New("an admitted relocation cannot be discarded")
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		return tx.PutBinding(retiredRelocationKind, id, struct {
			Reason string
			At     time.Time
		}{reason, s.now().UTC()})
	})
}

// RelocationWorkspaces pins prepared directories while a user decision waits.
// Invalidated unused plans are omitted and normal orphan sweeping can retire
// those directories; admitted work remains owned by its attempt/task binding.
func (s *Service) RelocationWorkspaces(ctx context.Context, node string) (map[string]bool, error) {
	plans, err := s.RelocationsFor(ctx, "")
	if err != nil {
		return nil, err
	}
	keep := map[string]bool{}
	for _, p := range plans {
		if p.Target.Node == node {
			keep[p.Target.Workspace.Path] = true
		}
	}
	return keep, nil
}

// OpenRelocation retires the old writer, acquires replacement leases and
// creates the new attempt with its recovery relationship in one transaction.
// The caller has verified actual complete checkpoint bytes and fresh target
// admission; these checks do not turn content metadata into executable bytes.
func (s *Service) OpenRelocation(ctx context.Context, planID string, approval RelocationApproval) (Record, error) {
	var created Record
	err := s.l.Update(ctx, func(tx *ledger.Tx) error {
		p, err := relocationPlanTx(tx, planID)
		if err != nil {
			return err
		}
		old, replacement, err := relocationRecordsTx(tx, p)
		if err != nil {
			return err
		}
		if replacement != nil {
			created = *replacement
			return nil
		}
		tracked, err := relocationTaskTx(tx, old, p.Owner)
		if err != nil {
			return err
		}
		spec := p.Target
		if err := s.checkReplacementSpec(spec, old, p); err != nil {
			return err
		}
		evidence, err := s.relocationStopEvidence(old, tracked, p, approval)
		if err != nil {
			return err
		}
		oldPhase := old.State
		if err := s.supersedeSourceTx(tx, &old, spec.ID, evidence); err != nil {
			return err
		}
		leases, err := s.leaseReplacementTx(tx, spec)
		if err != nil {
			return err
		}
		spec.Recovery = &RecoveryOrigin{AttemptID: old.ID, PlanID: p.ID, Checkpoint: p.Checkpoint}
		settled := true
		created = Record{Spec: spec, State: Leased, Revision: 1, SessionSettled: &settled, Leases: leases, StartedAt: s.now().UTC()}
		return s.recordRelocationTx(tx, p, approval, old, oldPhase, created)
	})
	return created, err
}

// relocationPlanTx loads an approved plan that has not been invalidated
// and still matches the fingerprint it was recorded under.
func relocationPlanTx(tx *ledger.Tx, planID string) (RelocationIntent, error) {
	var retired string
	if err := tx.QueryRow(`SELECT data FROM bindings WHERE kind = ? AND id = ?`, retiredRelocationKind, planID).Scan(&retired); err == nil {
		return RelocationIntent{}, errors.New("relocation plan was invalidated")
	} else if !errors.Is(err, sql.ErrNoRows) {
		return RelocationIntent{}, err
	}
	var raw string
	if err := tx.QueryRow(`SELECT data FROM bindings WHERE kind = ? AND id = ?`, relocationKind, planID).Scan(&raw); err != nil {
		return RelocationIntent{}, err
	}
	var p RelocationIntent
	if json.Unmarshal([]byte(raw), &p) != nil || p.ID != planID || p.Fingerprint() != planID {
		return RelocationIntent{}, errors.New("relocation plan changed")
	}
	return p, nil
}

// relocationRecordsTx finds the plan's attempts among the operations. A
// replacement an earlier call already created makes this one idempotent:
// it comes back as is and the source is not looked at. Otherwise the
// source must be the revision the plan was made from, still relocatable,
// and still executing under the plan's task epoch.
func relocationRecordsTx(tx *ledger.Tx, p RelocationIntent) (source Record, replacement *Record, err error) {
	ops, err := tx.Operations(kind, "")
	if err != nil {
		return Record{}, nil, err
	}
	found := false
	for _, op := range ops {
		if op.ID == p.Target.ID {
			existing, err := decode(op)
			if err != nil {
				return Record{}, nil, err
			}
			if existing.Recovery == nil || existing.Recovery.PlanID != p.ID {
				return Record{}, nil, ledger.ErrConflict
			}
			return Record{}, &existing, nil
		}
		if op.ID == p.SourceID {
			source, err = decode(op)
			if err != nil {
				return Record{}, nil, err
			}
			found = true
		}
	}
	if !found || source.Revision != p.SourceRevision || !Relocatable(source) || source.Execution.Epoch != p.TaskEpoch {
		return Record{}, nil, ledger.ErrConflict
	}
	if err := task.CheckExecutionTx(tx, source.Execution); err != nil {
		return Record{}, nil, err
	}
	return source, nil, nil
}

// relocationTaskTx is the task the source attempt runs for, which must
// still hold work and, when it names a requester, be the plan owner's.
func relocationTaskTx(tx *ledger.Tx, old Record, owner string) (*task.Task, error) {
	taskRaw, ok, err := tx.LoadDocument("tasks")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("task record missing")
	}
	var taskDocument struct {
		Tasks map[string]*task.Task `json:"tasks"`
	}
	if err := json.Unmarshal(taskRaw, &taskDocument); err != nil {
		return nil, err
	}
	tracked := taskDocument.Tasks[old.TaskID]
	if tracked == nil || !tracked.State.Holds() || (tracked.Requester != "" && tracked.Requester != owner) {
		return nil, errors.New("relocation task is no longer active or belongs to another requester")
	}
	return tracked, nil
}

// checkReplacementSpec is the shape a replacement must have: the
// original's task, turn, project and kind, on another node, in an isolated
// worktree there, based on the plan's checkpoint, under the same execution
// token one generation on, with a native command of its own. A region it
// names must be this ledger's.
func (s *Service) checkReplacementSpec(spec Spec, old Record, p RelocationIntent) error {
	if spec.TaskID != old.TaskID || spec.TurnID != old.TurnID || spec.Project != old.Project || spec.Kind != old.Kind || spec.Node == "" || spec.Node == old.Node || spec.Workspace.Node != spec.Node || spec.Workspace.Kind != project.KindWorktree || spec.Scope != ScopePathSet || spec.Base != p.Checkpoint || spec.Execution == nil || *spec.Execution != *old.Execution || spec.ExecutionGeneration != SessionExecutionEpoch(old)+1 || spec.NativeCommandID == "" || spec.NativeCommandID == InputCommandID(old) {
		return errors.New("replacement does not match its original task and isolated workspace")
	}
	if spec.Region != "" && spec.Region != s.l.Region() {
		return ledger.ErrUnknownRegion
	}
	return nil
}

// relocationStopEvidence establishes that the original stopped and names
// the evidence: the node's own receipt of its command left undispatched,
// observed within the last minute, or the plan owner's explicit
// plan-scoped confirmation of the stop and of the effects reviewed. The
// checkpoint rules then decide whether that evidence, with the plan's
// unresolved actions, admits a new attempt on the target.
func (s *Service) relocationStopEvidence(old Record, tracked *task.Task, p RelocationIntent, approval RelocationApproval) (string, error) {
	automatic := false
	if proof := approval.Node; proof != nil && !proof.ObservedAt.IsZero() && !proof.ObservedAt.After(s.now().Add(time.Second)) && s.now().Sub(proof.ObservedAt) <= time.Minute {
		st, cmd := proof.Session, proof.Session.Command
		automatic = st.Harness == old.Harness && st.ID == old.Session && strings.HasPrefix(st.ID, "ns_") && st.Binding.AttemptID == old.ID && st.Binding.TaskID == old.TaskID && st.Binding.TaskEpoch == old.Execution.Epoch && st.Binding.NodeID == old.Node && st.Binding.ProjectID == old.Project && st.Binding.SessionID == RetainedSessionID(tracked.Channel, tracked.ID, old.Agent) && st.Binding.ExecutionEpoch == SessionExecutionEpoch(old) && st.ProcessStopped && cmd != nil && cmd.ID == InputCommandID(old) && cmd.InputSequence > 0 && cmd.InputSequence <= st.InputAccepted && cmd.DispatchState == "not-dispatched" && cmd.ProcessStopped && !cmd.CancelRequested && cmd.State != nodewire.SessionCommandCancelled && !cmd.Settled
	}
	manual := approval.PlanID == p.ID && approval.Actor == p.Owner && approval.ChoiceID == "confirm-stopped-and-retry:"+p.ID && approval.StoppedConfirmed && approval.EffectsReviewed
	if !automatic && !manual {
		return "", errors.New("this relocation requires explicit plan-scoped stop and effects confirmation")
	}
	// Checkpoint identity is the logical task session, including when a
	// replacement failed before a native session was committed. Physical
	// stop evidence above still requires the exact original ns_ receipt.
	source := checkpoint.Source{TaskID: old.TaskID, SessionID: RetainedSessionID(tracked.Channel, tracked.ID, old.Agent), AttemptID: old.ID, TurnID: old.TurnID, NodeID: old.Node, ExecutionEpoch: SessionExecutionEpoch(old), TaskEpoch: old.Execution.Epoch}
	evidence := "operator-confirmation/" + p.ID
	if automatic {
		evidence = "node-undispatched/" + p.ID
	}
	var retry *checkpoint.RetryAuthorization
	if manual {
		retry = &checkpoint.RetryAuthorization{PlanID: p.ID, TaskID: old.TaskID, AttemptID: old.ID, Actor: approval.Actor, DecisionID: approval.ChoiceID, TargetNodeID: p.Target.Node}
	}
	decision := checkpoint.CheckReplacement(checkpoint.ReplacementRequest{PlanID: p.ID, Source: source, Isolation: checkpoint.IsolationEvidence{AttemptID: old.ID, ExecutionEpoch: source.ExecutionEpoch, Kind: checkpoint.IsolationProcessStopped, Reference: evidence}, Reconciliation: checkpoint.ActionReconciliation{AttemptID: old.ID, ExecutionEpoch: source.ExecutionEpoch, IsolationReference: evidence, Checked: true, Results: approval.ActionResults, RetryAuthorization: retry}, UnknownActions: p.UnknownActions, Target: checkpoint.ResumeTarget{NodeID: p.Target.Node, ExecutionEpoch: p.Target.ExecutionGeneration, TaskEpoch: p.TaskEpoch, Authorized: true, AdmissionChecked: true}})
	if decision.Action != checkpoint.ResumeStartAttempt {
		return "", errors.New(decision.Question.Message())
	}
	return evidence, nil
}

// supersedeSourceTx retires the original in place: its leases are retired
// as stopped and its record moves to superseded, one revision on, naming
// the replacement and the evidence the stop rests on.
func (s *Service) supersedeSourceTx(tx *ledger.Tx, old *Record, replacement, evidence string) error {
	if err := tx.RetireStopped(old.Leases); err != nil {
		return err
	}
	old.State = Superseded
	old.SupersededBy = replacement
	old.Unsettled = false
	old.StopEvidence = evidence
	settled := true
	old.SessionSettled = &settled
	old.Revision++
	old.EndedAt = s.now().UTC()
	data, err := json.Marshal(*old)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE operations SET state = ?, revision = ?, data = ?, updated_at = ? WHERE id = ?`, string(Superseded), old.Revision, string(data), old.EndedAt.Format(time.RFC3339Nano), old.ID)
	return err
}

// leaseReplacementTx admits the replacement and takes its leases in the
// same transaction: its attempt and workspace keys, then one slot at its
// endpoint when the harness counts them.
func (s *Service) leaseReplacementTx(tx *ledger.Tx, spec Spec) ([]ledger.Lease, error) {
	if err := checkAdmissionTx(tx, spec); err != nil {
		return nil, err
	}
	var leases []ledger.Lease
	for _, key := range []string{"attempt:" + spec.ID, "workspace:" + spec.Workspace.ID} {
		lease, err := tx.AcquireLocal(key, spec.ID, s.TTL)
		if err != nil {
			return nil, err
		}
		leases = append(leases, lease)
	}
	if spec.Slots > 0 {
		selected := false
		for slot := 1; slot <= spec.Slots; slot++ {
			lease, err := tx.AcquireLocal(fmt.Sprintf("%s:slot:%d", endpointKey(spec.Node, spec.Harness), slot), spec.ID, s.TTL)
			if err == nil {
				leases = append(leases, lease)
				selected = true
				break
			}
			if !errors.Is(err, ledger.ErrHeld) {
				return nil, err
			}
		}
		if !selected {
			return nil, NoSlot{Endpoint: endpointKey(spec.Node, spec.Harness), Slots: spec.Slots}
		}
	}
	return leases, nil
}

// recordRelocationTx writes the replacement's record and the open's two
// events — the original superseded from the phase it was in, the
// replacement leased — at the replacement's start, each carrying the plan
// and the approval the open rests on.
func (s *Service) recordRelocationTx(tx *ledger.Tx, p RelocationIntent, approval RelocationApproval, old Record, oldPhase State, created Record) error {
	data, err := json.Marshal(created)
	if err != nil {
		return err
	}
	stamp := created.StartedAt.Format(time.RFC3339Nano)
	if _, err := tx.Exec(`INSERT INTO operations(id, kind, state, revision, incarnation, data, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, created.ID, kind, string(Leased), 1, s.l.Incarnation(), string(data), stamp, stamp); err != nil {
		return err
	}
	proof, err := json.Marshal(struct {
		Plan     string             `json:"plan"`
		Approval RelocationApproval `json:"approval"`
	}{p.ID, approval})
	if err != nil {
		return err
	}
	for _, event := range []struct {
		id       string
		rev      int64
		from, to State
	}{{old.ID, old.Revision, oldPhase, Superseded}, {created.ID, 1, "", Leased}} {
		if _, err := tx.Exec(`INSERT INTO events(operation_id, revision, incarnation, from_state, to_state, actor, fencings, effects, at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, event.id, event.rev, s.l.Incarnation(), string(event.from), string(event.to), p.Owner, "[]", string(proof), stamp); err != nil {
			return err
		}
	}
	return nil
}
