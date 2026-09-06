package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/gopact-ai/steve/internal/ledger"
)

// ExecutionToken names the permission under which work began. Explicit stops
// revoke the old epoch even if this task is later resumed.
type ExecutionToken struct {
	TaskID string `json:"task_id"`
	Epoch  uint64 `json:"epoch"`
}

var ErrExecutionStopped = errors.New("task execution was stopped")

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
	t, ok := tasks[token.TaskID]
	if !ok || t.ExecutionEpoch != token.Epoch || t.State == StatePaused || t.State == StateCancelled {
		return fmt.Errorf("%w: task %s epoch %d", ErrExecutionStopped, token.TaskID, token.Epoch)
	}
	lineage, err := taskLineage(tasks, token.TaskID)
	if err != nil {
		return err
	}
	for _, ancestor := range lineage {
		if ancestor.State == StatePaused || ancestor.State == StateCancelled {
			return fmt.Errorf("%w: ancestor %s", ErrExecutionStopped, ancestor.ID)
		}
	}
	return nil
}

// CheckExecutionTx checks the task document in the same ledger transaction as
// an execution's result binding or landing admission.
func CheckExecutionTx(tx *ledger.Tx, token *ExecutionToken) error {
	if token == nil {
		return nil
	} // operations with no task owner
	raw, ok, err := tx.LoadDocument("tasks")
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: task document missing", ErrExecutionStopped)
	}
	var current data
	if err := json.Unmarshal(raw, &current); err != nil {
		return fmt.Errorf("read task execution: %w", err)
	}
	return checkExecution(current.Tasks, *token)
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
	next := s.clone()
	selected := map[string]bool{id: true}
	for changed := true; changed; {
		changed = false
		for childID, child := range next.Tasks {
			if !selected[childID] && selected[child.Parent] {
				selected[childID] = true
				changed = true
			}
		}
	}
	ids := make([]string, 0, len(selected))
	for taskID := range selected {
		t := next.Tasks[taskID]
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

// AdvanceExecution projects an outcome only if its original authorization
// still holds. A stopped task cannot be resurrected by a late successful call.
func (s *Store) AdvanceExecution(token ExecutionToken, to State) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := checkExecution(s.data.Tasks, token); err != nil {
		return Task{}, err
	}
	next := s.clone()
	stored := next.Tasks[token.TaskID]
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
