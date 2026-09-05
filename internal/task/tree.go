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
// Budget is a property of the tree, not of each task alone. A child is given
// the parent's *remaining* turns and time as its ceiling, so delegation can
// never spend more than the work it belongs to was allowed — without this,
// handing work to another agent would be the one way around the brake.
func (s *Store) Spawn(parentID string, child Task) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	parent, ok := s.data.Tasks[parentID]
	if !ok {
		return Task{}, fmt.Errorf("parent task %s not found", parentID)
	}
	if parent.State.Terminal() {
		return Task{}, fmt.Errorf("parent task %s is %s; nothing can be delegated from it", parentID, parent.State)
	}
	if limit, spent := parent.Budget.Exhausted(); spent {
		return Task{}, fmt.Errorf("parent task %s budget exhausted: %s", parentID, limit)
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
	child.CreatedAt = now
	child.UpdatedAt = now
	if child.Channel == "" {
		child.Channel = parent.Channel
	}
	if child.Requester == "" {
		child.Requester = parent.Requester
	}
	// A child answers where its parent does: the delivery goes back into
	// the conversation that asked, whichever channel that is.
	if child.Anchor.Zero() {
		child.Anchor = parent.Anchor
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
	if err := s.replaceLocked(next); err != nil {
		return Task{}, err
	}
	return child, nil
}

// Charge folds a finished child's spend into its parent, so what the child
// used is no longer available to anyone else in the tree. It walks up: a
// grandchild's cost reaches the root.
func (s *Store) Charge(childID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	child, ok := s.data.Tasks[childID]
	if !ok {
		return fmt.Errorf("task %s not found", childID)
	}
	if child.Parent == "" {
		return nil
	}
	next := s.clone()
	now := s.now()
	spent := child.Budget
	for cursor := next.Tasks[child.Parent]; cursor != nil; {
		cursor.Budget.Turns += spent.Turns
		cursor.Budget.Elapsed += spent.Elapsed
		cursor.Budget.ToolCalls += spent.ToolCalls
		cursor.Budget.Tokens = cursor.Budget.Tokens.Add(spent.Tokens)
		cursor.UpdatedAt = now
		if cursor.Parent == "" {
			break
		}
		cursor = next.Tasks[cursor.Parent]
	}
	return s.replaceLocked(next)
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
