package task

import (
	"errors"
	"fmt"
	"sort"
)

// ErrExecuting is work that cannot be deleted yet: an attempt of it is
// still open, so the caller stops it first.
var ErrExecuting = errors.New("task is executing")

// DeleteChannel removes what one conversation opened: the tasks it holds
// and everything delegated from them, with their organization. A task is
// the record of work asked for in a thread, so it goes when the thread
// goes; leaving it behind would list work nobody can open any more.
//
// Work in flight is never deleted out from under itself: a task with an
// open execution refuses, and the caller stops it first.
func (s *Store) DeleteChannel(channel string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	doomed := map[string]bool{}
	for id, stored := range s.data.Tasks {
		if stored.Channel == channel {
			doomed[id] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for id, stored := range s.data.Tasks {
			if doomed[id] || stored.Parent == "" || !doomed[stored.Parent] {
				continue
			}
			doomed[id] = true
			changed = true
		}
	}
	ids := make([]string, 0, len(doomed))
	for id := range doomed {
		if s.data.Tasks[id].HasOpenExecution() {
			return nil, fmt.Errorf("%w: %s", ErrExecuting, id)
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return lessID(ids[i], ids[j]) })
	if len(ids) == 0 {
		return ids, nil
	}
	next := s.clone()
	for _, id := range ids {
		delete(next.Tasks, id)
		delete(next.Meta, id)
	}
	if err := s.replaceLocked(next); err != nil {
		return nil, err
	}
	return ids, nil
}
