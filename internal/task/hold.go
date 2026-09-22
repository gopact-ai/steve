package task

import (
	"fmt"
	"time"
)

// Held says the user stopped the task and nothing has been said to it since.
func (t Task) Held() bool { return !t.HeldAt.IsZero() }

// Hold records that the user stopped the task's work. The state and the
// execution epoch are untouched: the task is still theirs to continue, and
// the next message continues it. The hold is persisted on purpose — a stop
// is the user's word, and the start-up delivery pass must not overrule it.
func (s *Store) Hold(id string) (Task, error) {
	return s.setHold(id, s.now())
}

// ReleaseHold lifts the hold once a turn of the task is being composed:
// from then on children ending are delivered again as they end.
func (s *Store) ReleaseHold(id string) (Task, error) {
	return s.setHold(id, time.Time{})
}

func (s *Store) setHold(id string, at time.Time) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.data.Tasks[id]
	if !ok {
		return Task{}, fmt.Errorf("task %s not found", id)
	}
	if stored.Held() == !at.IsZero() {
		// Already as asked: a second stop keeps the first one's time.
		return *stored.clone(), nil
	}
	next := s.clone()
	held := next.Tasks[id]
	held.HeldAt = at
	held.UpdatedAt = s.now()
	if err := s.replaceLocked(next); err != nil {
		return Task{}, err
	}
	return *held.clone(), nil
}
