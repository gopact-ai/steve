package task

// State observers receive the transition recorded by this write, not a later
// read of a task that may already have changed again. Metadata and progress
// writes still reach the ordinary observer but do not wake task recovery.
func (s *Store) notifyStateChangesLocked(next *draft, changes []recordChange) {
	if s.observeState == nil {
		return
	}
	type change struct {
		id    string
		state State
	}
	var states []change
	seen := map[string]bool{}
	for _, record := range changes {
		if record.kind != taskKind || seen[record.id] {
			continue
		}
		seen[record.id] = true
		before := s.data.Tasks[record.id]
		after, _ := next.find(record.id)
		if before != nil && after != nil && before.State != after.State {
			states = append(states, change{record.id, after.State})
		}
	}
	if len(states) > 0 {
		observe := s.observeState
		go func() {
			for _, state := range states {
				observe(state.id, state.state)
			}
		}()
	}
}
