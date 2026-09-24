package task

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
)

// ExecutionToken names the permission under which work began. Explicit stops
// revoke the old epoch even if this task is later resumed.
type ExecutionToken struct {
	TaskID string `json:"task_id"`
	Epoch  uint64 `json:"epoch"`
}

var ErrExecutionStopped = errors.New("task execution was stopped")

// ErrSettleState guards the one state a settlement means anything in: a task
// that failed. Running work is stopped, not settled.
var ErrSettleState = errors.New("only a failed task can be settled by hand")

func (s *Store) ExecutionToken(id string) (ExecutionToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.data.Tasks[id]
	if !ok {
		return ExecutionToken{}, fmt.Errorf("task %s not found", id)
	}
	if !t.State.Holds() {
		return ExecutionToken{}, fmt.Errorf("%w: task %s is %s", ErrExecutionStopped, id, t.State)
	}
	token := ExecutionToken{TaskID: id, Epoch: t.ExecutionEpoch}
	if err := checkExecution(s.data.Tasks, token); err != nil {
		return ExecutionToken{}, err
	}
	return token, nil
}

// CheckExecution allows already accepted results to land after normal task
// completion. Only an explicit stop changes their authorization.
func (s *Store) CheckExecution(token ExecutionToken) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return checkExecution(s.data.Tasks, token)
}

func checkExecution(tasks map[string]*Task, token ExecutionToken) error {
	return checkExecutionBy(func(id string) (*Task, bool) {
		t, ok := tasks[id]
		return t, ok
	}, token)
}

func checkExecutionBy(find func(string) (*Task, bool), token ExecutionToken) error {
	t, ok := find(token.TaskID)
	if !ok || t.ExecutionEpoch != token.Epoch || t.State == StatePaused || t.State == StateCancelled || t.CompletedByUser || t.Settled() {
		return fmt.Errorf("%w: task %s epoch %d", ErrExecutionStopped, token.TaskID, token.Epoch)
	}
	lineage, err := lineageOf(find, token.TaskID)
	if err != nil {
		return err
	}
	for _, ancestor := range lineage {
		if ancestor.State == StatePaused || ancestor.State == StateCancelled || ancestor.CompletedByUser || ancestor.Settled() {
			return fmt.Errorf("%w: ancestor %s", ErrExecutionStopped, ancestor.ID)
		}
	}
	return nil
}

// CheckExecutionTx reads the task and ancestor headers in the caller's
// transaction. No task histories or shared JSON documents are decoded.
func CheckExecutionTx(tx ledger.Reader, token *ExecutionToken) error {
	if token == nil {
		return nil
	}
	tasks := map[string]*Task{}
	id := token.TaskID
	for id != "" {
		if _, seen := tasks[id]; seen {
			return fmt.Errorf("task %s has cyclic ancestry", id)
		}
		tracked, found, err := GetTx(tx, id)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%w: task %s missing", ErrExecutionStopped, id)
		}
		tasks[id] = &tracked
		id = tracked.Parent
	}
	return checkExecution(tasks, *token)
}

// SetAside revokes the task's execution and its descendants atomically with
// their visible state. Spawn shares this lock, so a child cannot escape a stop.
// Finished descendants keep their result, but lose permission to land it later.
func (s *Store) SetAside(id string, to State) ([]string, error) {
	if to != StatePaused && to != StateCancelled {
		return nil, fmt.Errorf("invalid task stop state %s", to)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data.Tasks[id]; !ok {
		return nil, fmt.Errorf("task %s not found", id)
	}
	next := s.draft()
	selected := map[string]bool{id: true}
	for changed := true; changed; {
		changed = false
		for childID, child := range s.data.Tasks {
			if !selected[childID] && selected[child.Parent] {
				selected[childID] = true
				changed = true
			}
		}
	}
	ids := make([]string, 0, len(selected))
	for taskID := range selected {
		t := next.edit(taskID)
		t.ExecutionEpoch++
		if !t.State.Terminal() && (t.State.CanMoveTo(to) || t.State == to) {
			t.State = to
		}
		t.UpdatedAt = s.now()
		ids = append(ids, taskID)
	}
	if err := s.replaceLocked(next); err != nil {
		return nil, err
	}
	sort.Strings(ids)
	return ids, nil
}

// Settle records a person closing a failed task by hand, or clearing that
// decision with an empty settlement. The state is left alone: the record has
// to keep saying the work failed. The execution epoch moves for the same
// reason it moves on a stop — a late result must not revive a task its owner
// has already closed.
func (s *Store) Settle(id string, as Settlement) (Task, error) {
	if as != "" && !as.Valid() {
		return Task{}, fmt.Errorf("invalid settlement %s", as)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.data.Tasks[id]
	if !ok {
		return Task{}, fmt.Errorf("task %s not found", id)
	}
	if stored.State != StateFailed {
		return Task{}, fmt.Errorf("%w: task %s is %s", ErrSettleState, id, stored.State)
	}
	next := s.draft()
	t := next.edit(id)
	t.Settlement = as
	t.SettledAt = time.Time{}
	if as != "" {
		t.SettledAt = s.now()
		t.ExecutionEpoch++
	}
	t.UpdatedAt = s.now()
	if err := s.replaceLocked(next); err != nil {
		return Task{}, err
	}
	return *t.clone(), nil
}

// AdvanceExecution projects an outcome only if its original authorization
// still holds. A stopped task cannot be resurrected by a late successful call.
func (s *Store) AdvanceExecution(token ExecutionToken, to State) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := checkExecution(s.data.Tasks, token); err != nil {
		return Task{}, err
	}
	next := s.draft()
	stored := next.edit(token.TaskID)
	if !stored.State.CanMoveTo(to) {
		return Task{}, fmt.Errorf("task %s cannot move %s -> %s", token.TaskID, stored.State, to)
	}
	stored.State = to
	stored.UpdatedAt = s.now()
	if err := s.replaceLocked(next); err != nil {
		return Task{}, err
	}
	return *stored.clone(), nil
}
