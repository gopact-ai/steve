package task

import (
	"errors"
	"fmt"

	"github.com/gopact-ai/steve/internal/ledger"
)

// ResumeAdmission binds one durable channel input to one task incarnation.
// The channel carries this reference; only the task owner grants authority.
type ResumeAdmission struct {
	ID     string `json:"id"`
	TaskID string `json:"task_id"`
	Epoch  uint64 `json:"epoch"`
}

// ResumeGrant is committed with Resume, and consumed with BeginTurn's charge.
// TurnID retains the original execution identity, never permission to replay it.
type ResumeGrant struct {
	Admission ResumeAdmission `json:"admission"`
	Consumed  bool            `json:"consumed"`
	TurnID    string          `json:"turn_id,omitempty"`
}

var (
	ErrResumePending  = errors.New("task resume has not been authorized")
	ErrResumeConsumed = errors.New("task resume was already consumed")
)

func (a ResumeAdmission) Valid() bool { return a.ID != "" && a.TaskID != "" && a.Epoch != 0 }

func checkResumeAdmission(t Task, a ResumeAdmission) error {
	if !a.Valid() || t.ID != a.TaskID {
		return fmt.Errorf("%w: invalid resume admission", ErrExecutionStopped)
	}
	if t.ResumeGrant.Admission == a && t.ResumeGrant.Consumed {
		return ErrResumeConsumed
	}
	if t.ExecutionEpoch == a.Epoch && t.ResumeGrant.Admission == a &&
		t.State == StateRunning && !t.CompletedByUser && !t.Settled() {
		return nil
	}
	if a.Epoch > t.ExecutionEpoch && a.Epoch-t.ExecutionEpoch == 1 &&
		t.State.CanMoveTo(StateRunning) && !t.Settled() {
		return ErrResumePending
	}
	return fmt.Errorf("%w: resume %s no longer owns task %s", ErrExecutionStopped, a.ID, a.TaskID)
}

// CheckResumeAdmissionTx is a bounded owner read for channel dispatch. Final
// consumption still belongs to BeginTurn; this check alone grants nothing.
func CheckResumeAdmissionTx(tx ledger.Reader, a ResumeAdmission) error {
	t, found, err := GetTx(tx, a.TaskID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: task %s missing", ErrExecutionStopped, a.TaskID)
	}
	if err := checkResumeAdmission(t, a); err != nil {
		return err
	}
	return CheckExecutionTx(tx, &ExecutionToken{TaskID: a.TaskID, Epoch: a.Epoch})
}

func (s *Store) CheckResumeAdmission(a ResumeAdmission) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, found := s.data.Tasks[a.TaskID]
	if !found {
		return fmt.Errorf("%w: task %s missing", ErrExecutionStopped, a.TaskID)
	}
	if err := checkResumeAdmission(*t, a); err != nil {
		return err
	}
	return checkExecution(s.data.Tasks, ExecutionToken{TaskID: a.TaskID, Epoch: a.Epoch})
}
