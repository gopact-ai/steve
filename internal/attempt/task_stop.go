package attempt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/task"
)

const taskStopReceiptKind = "attempt-task-stop"

var errTaskStopRecorded = errors.New("task stop evidence already recorded")

// TaskStopReceipt is a node's observed outcome for the original execution.
// It records a fact, not a new execution grant or a replacement task intent.
type TaskStopReceipt struct {
	AttemptID string           `json:"attempt_id"`
	Kind      string           `json:"kind"`
	Evidence  RetainedEvidence `json:"evidence"`
}

func (s *Service) TaskStopReceipt(ctx context.Context, id string) (TaskStopReceipt, bool, error) {
	var receipt TaskStopReceipt
	ok, err := s.l.GetBinding(ctx, taskStopReceiptKind, id, &receipt)
	return receipt, ok, err
}

// ConfirmTaskStopped consumes a fresh native receipt only after the original
// task token was explicitly revoked. Normal task completion is not revocation.
// Native evidence, quarantine removal and retirement of the exact old leases
// commit together; no new task/turn is finished or admitted by this operation.
func (s *Service) ConfirmTaskStopped(ctx context.Context, id, actor string, proof RetainedEvidence) (Record, error) {
	current, err := s.Get(ctx, id)
	if err != nil {
		return Record{}, err
	}
	if current.StopEvidence == "task-stop/"+id && taskStopAlreadySettled(current) {
		return current, nil
	}
	if actor == "" || proof.ObservedAt.IsZero() || proof.ObservedAt.After(s.now().Add(time.Second)) || s.now().Sub(proof.ObservedAt) > time.Minute {
		return Record{}, errors.New("task stop requires fresh native evidence and an actor")
	}
	to := current.State
	if !to.Terminal() {
		to = Failed
	}
	var next Record
	_, err = s.l.Transition(ctx, id, string(current.State), string(to), actor, nil, map[string]string{"stop_evidence": "task-stop/" + id}, func(tx *ledger.Tx, op *ledger.Operation) error {
		if err := json.Unmarshal(op.Data, &next); err != nil {
			return err
		}
		tracked, err := stoppedTaskTx(tx, next)
		if err != nil {
			return err
		}
		if taskStopAlreadySettled(next) {
			return errTaskStopRecorded
		}
		st := proof.Session
		if !matchesStoppedSession(next, tracked, st) {
			return errors.New("native stop receipt belongs to another execution")
		}
		kind := "native-command-settled"
		if st.ProcessStopped {
			kind = "native-process-stopped"
		} else if st.Command == nil || !st.Command.Settled || (st.Command.State != "completed" && st.Command.State != "cancelled") {
			return ErrStopConfirmationRequired
		}
		if err := tx.PutBinding(taskStopReceiptKind, next.ID, TaskStopReceipt{AttemptID: next.ID, Kind: kind, Evidence: proof}); err != nil {
			return err
		}
		if err := tx.RetireStopped(next.Leases); err != nil {
			return err
		}
		settled := true
		next.Unsettled, next.SessionSettled = false, &settled
		next.StopEvidence = "task-stop/" + next.ID
		next.State, next.Revision = to, op.Revision+1
		next.Error = "已按用户的暂停或取消要求结束原执行。"
		if next.EndedAt.IsZero() {
			next.EndedAt = s.now().UTC()
		}
		u := st.Progress.Usage
		if u.Reported || u.InputTokens != 0 || u.OutputTokens != 0 || u.CacheReadTokens != 0 || u.CacheWriteTokens != 0 {
			model := st.Progress.Settings.Model
			if model == "" {
				model = st.Settings.Model
			}
			next.Usage = &Usage{Model: model, Input: stoppedTokenCount(u.InputTokens), Output: stoppedTokenCount(u.OutputTokens), CachedRead: stoppedTokenCount(u.CacheReadTokens), CachedWrite: stoppedTokenCount(u.CacheWriteTokens), Context: stoppedTokenCount(u.ContextTokens), Reported: u.Reported}
		}
		return tx.SetData(op, next)
	})
	if errors.Is(err, errTaskStopRecorded) {
		return s.Get(ctx, id)
	}
	return next, err
}

func stoppedTokenCount(value uint64) int64 { return int64(min(value, uint64(1<<63-1))) }

func taskStopAlreadySettled(r Record) bool {
	return r.State.Terminal() && !r.Unsettled && r.SessionSettled != nil && *r.SessionSettled
}

func stoppedTaskTx(tx *ledger.Tx, r Record) (task.Task, error) {
	if r.State == Superseded || r.Execution == nil || r.Execution.TaskID != r.TaskID || !strings.HasPrefix(r.Session, "ns_") {
		return task.Task{}, errors.New("task stop requires an original node-owned execution")
	}
	raw, ok, err := tx.LoadDocument("tasks")
	if err != nil || !ok {
		return task.Task{}, errors.Join(errors.New("task stop requires the original task record"), err)
	}
	var data struct {
		Tasks map[string]task.Task `json:"tasks"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return task.Task{}, err
	}
	tracked, ok := data.Tasks[r.TaskID]
	if !ok {
		return task.Task{}, errors.New("task stop source is missing")
	}
	err = task.CheckExecutionTx(tx, r.Execution)
	if !errors.Is(err, task.ErrExecutionStopped) {
		return task.Task{}, errors.Join(errors.New("original task execution has not been revoked"), err)
	}
	return tracked, nil
}

func matchesStoppedSession(r Record, tracked task.Task, st nodewire.SessionState) bool {
	if st.ID != r.Session || st.Harness != r.Harness || st.Binding.ProjectID != r.Project || st.Binding.NodeID != r.Node || st.Binding.TaskID != r.TaskID || st.Binding.AttemptID != r.ID || st.Binding.ExecutionEpoch != SessionExecutionEpoch(r) || st.Binding.TaskEpoch != r.Execution.Epoch || st.Binding.SessionID != RetainedSessionID(tracked.Channel, tracked.ID, r.Agent) {
		return false
	}
	return st.Command == nil && st.ProcessStopped || st.Command != nil && st.Command.ID == InputCommandID(r) && st.Command.InputSequence > 0 && st.Command.InputSequence <= st.InputAccepted
}

// TaskStopPending keeps lack of a native receipt visible without interpreting
// it as stopped. Repeated checks with the same explanation add no new event.
func (s *Service) TaskStopPending(ctx context.Context, id, actor, explanation string) error {
	current, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if taskStopAlreadySettled(current) {
		return nil
	}
	if explanation == "" || len(explanation) > 16<<10 {
		return errors.New("task stop pending explanation is invalid")
	}
	if current.Unsettled && current.Error == explanation {
		return nil
	}
	_, err = s.l.Transition(ctx, id, string(current.State), string(current.State), actor, nil, nil, func(tx *ledger.Tx, op *ledger.Operation) error {
		var next Record
		if err := json.Unmarshal(op.Data, &next); err != nil {
			return err
		}
		if taskStopAlreadySettled(next) {
			return errTaskStopRecorded
		}
		if _, err := stoppedTaskTx(tx, next); err != nil {
			return fmt.Errorf("record pending native stop: %w", err)
		}
		next.Unsettled, next.Error, next.Revision = true, explanation, op.Revision+1
		return tx.SetData(op, next)
	})
	if errors.Is(err, errTaskStopRecorded) {
		return nil
	}
	return err
}
