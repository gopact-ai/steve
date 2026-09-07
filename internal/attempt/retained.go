package attempt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/task"
)

// RetainedEvidence is a fresh authenticated attach response from the node
// that owns the original session. No prompt replay or heartbeat inference
// may create this evidence. It admits only observing that same execution.
type RetainedEvidence struct {
	ObservedAt time.Time
	Session    nodewire.SessionState
}

type RecoveryOrigin struct {
	AttemptID  string `json:"attempt_id"`
	PlanID     string `json:"plan_id"`
	Checkpoint string `json:"checkpoint"`
}

func InputCommandID(record Record) string {
	if record.NativeCommandID != "" {
		return record.NativeCommandID
	}
	if record.TurnID != "" {
		return record.TurnID
	}
	return record.ID
}

func SessionExecutionEpoch(record Record) uint64 {
	if record.ExecutionGeneration != 0 {
		return record.ExecutionGeneration
	}
	for _, lease := range record.Leases {
		if lease.Key == "attempt:"+record.ID {
			return lease.Epoch
		}
	}
	return 1
}

func retainedKind(kind Kind) bool {
	switch kind {
	case KindChat, KindDelegate, KindStep, KindPlan, KindVerify:
		return true
	}
	return false
}
func retainedPhase(state State) bool {
	switch state {
	case Running, Snapshotted, Published, Durable, Verifying, BindReady:
		return true
	}
	return false
}

// RecordSession records the native identity before Prompt without changing the
// attempt's phase. A session may be assigned once; it cannot be replaced under
// an already admitted attempt or written with an expired lease/task token.
func (s *Service) RecordSession(ctx context.Context, id, actor, session string) (Record, error) {
	if !strings.HasPrefix(session, "ns_") || len(session) > 512 || strings.ContainsAny(session, "\x00\r\n") {
		return Record{}, errors.New("a node-owned session identity is required")
	}
	r, err := s.Get(ctx, id)
	if err != nil {
		return Record{}, err
	}
	if r.State.Terminal() {
		return Record{}, ErrBadState
	}
	var saved Record
	_, err = s.l.Transition(ctx, id, string(r.State), string(r.State), actor, r.Leases, nil, func(tx *ledger.Tx, op *ledger.Operation) error {
		if err := json.Unmarshal(op.Data, &saved); err != nil {
			return err
		}
		if saved.Execution == nil || saved.Unsettled {
			return errors.New("session assignment requires an admitted execution")
		}
		if err := task.CheckExecutionTx(tx, saved.Execution); err != nil {
			return err
		}
		if saved.Session != "" && saved.Session != session {
			return errors.New("attempt session identity cannot change")
		}
		saved.Session = session
		saved.Revision = op.Revision + 1
		return tx.SetData(op, saved)
	})
	return saved, err
}

// RetainedSessionID binds the portable conversation identity to its agent.
func RetainedSessionID(channel, taskID, agentID string) string {
	if channel == "" {
		channel = "task:" + taskID
	}
	digest := sha256.Sum256([]byte(channel + "\x00" + agentID))
	return "conversation-" + hex.EncodeToString(digest[:])
}

// RecoverRetained reauthorizes observation/settlement of the original running
// chat or delegated attempt after a coordinator loss. All leases, the task epoch and the
// attached session's input receipt are checked in one ledger transaction.
func (s *Service) RecoverRetained(ctx context.Context, id string, evidence RetainedEvidence) (Record, error) {
	current, err := s.Get(ctx, id)
	if err != nil {
		return Record{}, err
	}
	if evidence.ObservedAt.IsZero() || evidence.ObservedAt.After(s.now().Add(time.Second)) || s.now().Sub(evidence.ObservedAt) > time.Minute {
		return Record{}, errors.New("retained session evidence is not fresh")
	}
	var result Record
	_, err = s.l.Transition(ctx, id, string(current.State), string(current.State), "retained-session", nil, map[string]any{"session": evidence.Session.ID, "binding": evidence.Session.Binding, "observed_at": evidence.ObservedAt, "node_state": evidence.Session.State}, func(tx *ledger.Tx, op *ledger.Operation) error {
		if err := json.Unmarshal(op.Data, &result); err != nil {
			return err
		}
		if !retainedPhase(State(op.State)) || !retainedKind(result.Kind) || result.Execution == nil {
			return errors.New("attempt is not a supported retained execution phase")
		}
		state, binding, command := evidence.Session, evidence.Session.Binding, evidence.Session.Command
		if !strings.HasPrefix(state.ID, "ns_") || state.ID != result.Session || state.Harness != result.Harness || binding.ProjectID != result.Project || binding.AttemptID != result.ID || binding.TaskID != result.TaskID || binding.NodeID != result.Node || binding.TaskEpoch != result.Execution.Epoch {
			return errors.New("retained node session does not match the admitted attempt")
		}
		if result.Execution.TaskID != result.TaskID {
			return errors.New("retained task execution identity differs")
		}
		if err := task.CheckExecutionTx(tx, result.Execution); err != nil {
			return err
		}
		raw, ok, err := tx.LoadDocument("tasks")
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("retained task record is missing")
		}
		var tasks struct {
			Tasks map[string]*task.Task `json:"tasks"`
		}
		if err := json.Unmarshal(raw, &tasks); err != nil {
			return err
		}
		tracked := tasks.Tasks[result.TaskID]
		if tracked == nil || binding.SessionID != RetainedSessionID(tracked.Channel, tracked.ID, result.Agent) {
			return errors.New("retained logical session identity differs")
		}
		own := false
		for _, lease := range result.Leases {
			if lease.Key == "attempt:"+result.ID {
				own = lease.Holder == result.ID && SessionExecutionEpoch(result) == binding.ExecutionEpoch
			}
		}
		if !own {
			return errors.New("retained execution lease differs")
		}
		commandID := InputCommandID(result)
		if command == nil || command.ID != commandID || command.InputSequence == 0 || command.InputSequence > state.InputAccepted {
			return errors.New("retained input receipt is missing or belongs to another command")
		}
		settled := (command.State == "completed" || command.State == "cancelled") && command.Settled
		live := state.State == "running" && !state.ProcessStopped && !command.ProcessStopped && (command.State == "accepted" || command.State == "running")
		if !settled && !live {
			return errors.New("original node execution cannot be confirmed live or settled")
		}
		if State(op.State) != Running && !settled {
			return errors.New("post-prompt phase requires a settled original native command")
		}
		for _, lease := range result.Leases {
			if lease.Holder != result.ID {
				return fmt.Errorf("retained attempt contains a different lease holder")
			}
		}
		leases, err := tx.RenewRetained(result.Leases, s.TTL)
		if err != nil {
			return err
		}
		result.Leases = leases
		result.Unsettled = false
		result.Error = ""
		result.SessionSettled = &settled
		result.Revision = op.Revision + 1
		return tx.SetData(op, result)
	})
	return result, err
}
