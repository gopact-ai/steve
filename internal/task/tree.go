package task

import (
	"fmt"
	"sort"
	"time"
)

// MaxDepth bounds delegation. A→B→C→D is already a chain nobody can follow
// in a chat; deeper than that is a plan that should have been written as one.
const MaxDepth = 4

// Spawn opens a child task funded from what the parent has left.
//
// A child inherits a ceiling from the parent's remaining budget. This is
// not a reservation: Begin checks and charges the shared ancestor budget
// atomically, so siblings cannot each consume the same remaining turns.
func (s *Store) Spawn(parentID string, child Task) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.spawnLocked(parentID, child, s.replaceLocked)
}

func (s *Store) spawnLocked(parentID string, child Task, replace func(data) error) (Task, error) {
	parent, ok := s.data.Tasks[parentID]
	if !ok {
		return Task{}, fmt.Errorf("parent task %s not found", parentID)
	}
	if !parent.State.Holds() {
		return Task{}, fmt.Errorf("parent task %s is %s; nothing can be delegated from it", parentID, parent.State)
	}
	if limit, spent := parent.Budget.Exhausted(); spent {
		return Task{}, fmt.Errorf("parent task %s budget exhausted: %s", parentID, limit)
	}
	lineage, err := taskLineage(s.data.Tasks, parentID)
	if err != nil {
		return Task{}, err
	}
	for _, ancestor := range lineage {
		if ancestor.State == StatePaused || ancestor.State == StateCancelled {
			return Task{}, fmt.Errorf("%w: ancestor %s", ErrExecutionStopped, ancestor.ID)
		}
	}
	depth := 1
	for cursor := parent; cursor.Parent != ""; depth++ {
		next, ok := s.data.Tasks[cursor.Parent]
		if !ok {
			break
		}
		cursor = next
	}
	if depth >= MaxDepth {
		return Task{}, fmt.Errorf("delegation is already %d deep under task %s; the limit is %d", depth, parentID, MaxDepth)
	}
	// A cycle is A asking B asking A: the member about to run this child is
	// already responsible for one of its ancestors. It is refused here, in
	// the structure, rather than left to a prompt to discourage.
	for cursor := parent; ; {
		if cursor.Member != "" && cursor.Member == child.Member {
			return Task{}, fmt.Errorf("%s already holds task %s above this one; delegating back to it would loop", child.Member, cursor.ID)
		}
		if cursor.Parent == "" {
			break
		}
		next, ok := s.data.Tasks[cursor.Parent]
		if !ok {
			break
		}
		cursor = next
	}

	now := s.now()
	child.ID = fmt.Sprintf("%d", s.data.NextID)
	child.Parent = parentID
	child.State = StateDraft
	child.ExecutionEpoch = 1
	child.CreatedAt = now
	child.UpdatedAt = now
	if child.Channel == "" {
		child.Channel = parent.Channel
	}
	if child.Requester == "" {
		child.Requester = parent.Requester
	}
	// The child's ceiling is what the parent has left, never more. A
	// smaller explicit ceiling is allowed: a caller may ration. A parent
	// without a ceiling (zero: unlimited) passes none on — its children
	// run as long as it does, unless the caller rations.
	if parent.Budget.MaxTurns > 0 {
		turnsLeft := parent.Budget.MaxTurns - parent.Budget.Turns
		if turnsLeft <= 0 {
			return Task{}, fmt.Errorf("parent task %s has no turns left to delegate", parentID)
		}
		if child.Budget.MaxTurns <= 0 || child.Budget.MaxTurns > turnsLeft {
			child.Budget.MaxTurns = turnsLeft
		}
	}
	if parent.Budget.MaxElapsed > 0 {
		timeLeft := parent.Budget.MaxElapsed - parent.Budget.Elapsed
		if timeLeft <= 0 {
			return Task{}, fmt.Errorf("parent task %s has no time left to delegate", parentID)
		}
		if child.Budget.MaxElapsed <= 0 || child.Budget.MaxElapsed > timeLeft {
			child.Budget.MaxElapsed = timeLeft
		}
	}

	next := s.clone()
	next.NextID = s.data.NextID + 1
	next.Tasks[child.ID] = &child
	if err := replace(next); err != nil {
		return Task{}, err
	}
	return child, nil
}

// taskLineage returns the task followed by its ancestors from a candidate
// store snapshot. Invalid ancestry refuses the whole budget mutation.
func taskLineage(tasks map[string]*Task, id string) ([]*Task, error) {
	var out []*Task
	seen := map[string]bool{}
	for id != "" {
		if seen[id] {
			return nil, fmt.Errorf("task %s has cyclic ancestry", id)
		}
		seen[id] = true
		member, ok := tasks[id]
		if !ok {
			return nil, fmt.Errorf("task %s not found in ancestry", id)
		}
		out = append(out, member)
		id = member.Parent
	}
	return out, nil
}

// Ancestry lists the tasks above id, nearest parent first. It is what a
// delegated step's context carries instead of transcripts: the goals, so the
// agent knows what its work serves.
func (s *Store) Ancestry(id string) []Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Task
	cursor, ok := s.data.Tasks[id]
	for ok && cursor.Parent != "" {
		cursor, ok = s.data.Tasks[cursor.Parent]
		if ok {
			out = append(out, *cursor.clone())
		}
	}
	return out
}

// Children lists a task's direct children, oldest first.
func (s *Store) Children(id string) []Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Task
	for _, stored := range s.data.Tasks {
		if stored.Parent == id {
			out = append(out, *stored.clone())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// Deadline is the wall-clock moment the task's time budget runs out if it
// keeps running from now — the number a delegated agent is told.
func (t Task) Deadline(now time.Time) time.Time {
	return now.Add(t.Budget.MaxElapsed - t.Budget.Elapsed)
}
