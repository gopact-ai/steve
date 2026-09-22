package task

import (
	"fmt"
	"time"
)

// Held says the user stopped the task and nothing has been said to it since.
func (t Task) Held() bool { return !t.HeldAt.IsZero() }

// Hold records that the user stopped the task's work, stamped with when.
// The state and the execution epoch are untouched: the task is still
// theirs to continue, and the next message continues it. The hold is
// persisted on purpose — a stop is the user's word, and the start-up
// delivery pass must not overrule it. A stop while held stamps it again:
// the release is bound to the stamp a turn saw when it composed, so a
// turn composed under the earlier stop cannot lift the later one.
func (s *Store) Hold(id string) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.data.Tasks[id]
	if !ok {
		return Task{}, fmt.Errorf("task %s not found", id)
	}
	at := s.now()
	if !at.After(stored.HeldAt) {
		// Two stops within the clock's resolution are still two stops.
		at = stored.HeldAt.Add(time.Nanosecond)
	}
	return s.setHoldLocked(id, at)
}

// ReleaseHold lifts the hold a turn accounted for: seen is the stamp the
// turn read when it composed. A hold stamped since is a later stop's, and
// stays; so does nothing, when the task is not held.
func (s *Store) ReleaseHold(id string, seen time.Time) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.data.Tasks[id]
	if !ok {
		return Task{}, fmt.Errorf("task %s not found", id)
	}
	if !stored.Held() || !stored.HeldAt.Equal(seen) {
		return *stored.clone(), nil
	}
	return s.setHoldLocked(id, time.Time{})
}

func (s *Store) setHoldLocked(id string, at time.Time) (Task, error) {
	next := s.clone()
	held := next.Tasks[id]
	held.HeldAt = at
	held.UpdatedAt = s.now()
	if err := s.replaceLocked(next); err != nil {
		return Task{}, err
	}
	return *held.clone(), nil
}
