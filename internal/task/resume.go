package task

import (
	"context"
	"fmt"
	"github.com/gopact-ai/steve/internal/ledger"
)

// Resume admits a channel's accepted re-entry only while the task still has
// the state and authority observed before contacting the channel. In
// particular, a second pause must not be undone by a late resume response.
func (s *Store) Resume(id string, epoch uint64, from State, admission ResumeAdmission) (Task, error) {
	return s.ResumeChecked(context.Background(), id, epoch, from, admission, nil)
}

func (s *Store) ResumeChecked(ctx context.Context, id string, epoch uint64, from State, admission ResumeAdmission, guard func(*ledger.Tx) error) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.data.Tasks[id]
	if ok && admission.Valid() && stored.ResumeGrant.Admission == admission {
		// An identical request reads its original grant, including a consumed
		// or revoked one. It never recreates permission after a later stop.
		if guard != nil {
			if err := s.book.Update(ctx, guard); err != nil {
				return Task{}, err
			}
		}
		return *stored.clone(), nil
	}
	if !ok || stored.ExecutionEpoch != epoch || stored.State != from {
		return Task{}, fmt.Errorf("%w: task %s changed during resume", ErrExecutionStopped, id)
	}
	if !from.CanMoveTo(StateRunning) || from == StateRunning || stored.Settled() || stored.HasOpenExecution() {
		return Task{}, fmt.Errorf("task %s cannot resume from %s", id, from)
	}
	if admission != (ResumeAdmission{}) && (!admission.Valid() || admission.TaskID != id || epoch == ^uint64(0) || admission.Epoch != epoch+1) {
		return Task{}, fmt.Errorf("%w: invalid resume grant for task %s", ErrExecutionStopped, id)
	}
	next := s.draft()
	resumed := next.edit(id)
	if admission.Valid() {
		resumed.ExecutionEpoch = admission.Epoch
		resumed.ResumeGrant = ResumeGrant{Admission: admission}
	} else if from == StatePaused {
		resumed.ExecutionEpoch++
	}
	resumed.State = StateRunning
	resumed.UpdatedAt = s.now()
	if err := checkExecutionBy(next.find, ExecutionToken{TaskID: id, Epoch: resumed.ExecutionEpoch}); err != nil {
		return Task{}, err
	}
	if err := s.replaceRecordsLocked(ctx, next, guard); err != nil {
		return Task{}, err
	}
	return *resumed.clone(), nil
}
