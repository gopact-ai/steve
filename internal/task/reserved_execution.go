package task

import (
	"errors"
	"fmt"
	"time"
)

// ReserveAttempt admits an independently driven execution, such as a parallel
// plan step or verifier. Its own accounting identity and every ancestor's turn
// charge commit together; it never binds or settles another open row.
func (s *Store) ReserveAttempt(token ExecutionToken, attemptID, turnID, member, node string, startedAt time.Time) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := checkExecution(s.data.Tasks, token); err != nil {
		return Task{}, err
	}
	if attemptID == "" || turnID == "" || member == "" {
		return Task{}, errors.New("independent execution accounting identity is required")
	}
	tracked := s.data.Tasks[token.TaskID]
	for _, row := range tracked.Attempts {
		if row.ExecutionID != attemptID {
			continue
		}
		if !row.Independent || row.TurnID != turnID || row.ExecutionEpoch != token.Epoch || row.Member != member || row.Node != node || !startedAt.IsZero() && !row.StartedAt.Equal(startedAt) {
			return Task{}, errors.New("independent execution accounting identity changed")
		}
		return *tracked.clone(), nil
	}
	if !tracked.State.Holds() || !tracked.State.CanMoveTo(StateRunning) {
		return Task{}, fmt.Errorf("task %s cannot admit execution from %s", token.TaskID, tracked.State)
	}
	next := s.clone()
	tracked = next.Tasks[token.TaskID]
	now := s.now()
	if startedAt.IsZero() {
		startedAt = now
	}
	if err := reserveTurn(next.Tasks, token.TaskID, now); err != nil {
		return Task{}, err
	}
	tracked.Attempts = append(tracked.Attempts, Attempt{Independent: true, ExecutionID: attemptID, TurnID: turnID, ExecutionEpoch: token.Epoch, Member: member, Node: node, StartedAt: startedAt})
	tracked.State = StateRunning
	if err := s.replaceLocked(next); err != nil {
		return Task{}, err
	}
	return *tracked.clone(), nil
}
