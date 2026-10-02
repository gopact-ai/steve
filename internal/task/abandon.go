package task

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
)

// AbandonExecution freezes one bound accounting row and cancels its task tree.
// record validates and records the related execution decision in the same
// transaction. No cache or observer sees either side of a rejected commit.
func (s *Store) AbandonExecution(ctx context.Context, taskID, executionID, turnID string, record func(*ledger.Tx, Attempt, time.Time) (RecoveryUsage, error)) error {
	if record == nil || executionID == "" {
		return errors.New("abandonment requires an exact execution and atomic record")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stored := s.data.Tasks[taskID]
	if stored == nil {
		return fmt.Errorf("task %s not found", taskID)
	}
	next := s.draft()
	selected := map[string]bool{taskID: true}
	for changed := true; changed; {
		changed = false
		for id, child := range s.data.Tasks {
			if !selected[id] && selected[child.Parent] {
				selected[id], changed = true, true
			}
		}
	}
	at := s.now().UTC()
	if stored.State != StateCancelled {
		for id := range selected {
			child := next.edit(id)
			child.ExecutionEpoch++
			if !child.State.Terminal() && (child.State.CanMoveTo(StateCancelled) || child.State == StateCancelled) {
				child.State = StateCancelled
			}
			child.UpdatedAt = at
		}
	}
	tracked := next.edit(taskID)
	var row *Attempt
	for index := range tracked.Attempts {
		if tracked.Attempts[index].ExecutionID == executionID {
			row = &tracked.Attempts[index]
			break
		}
	}
	if row == nil || row.TurnID != turnID {
		return errors.New("abandonment has no matching execution accounting row")
	}
	var changes []recordChange
	err := s.book.Update(ctx, func(tx *ledger.Tx) error {
		control, err := controlTx(tx)
		if err != nil {
			return err
		}
		if control.Revision != s.revision || control.NextID != s.data.NextID {
			return ledger.ErrConflict
		}
		usage, err := record(tx, *row, at)
		if err != nil {
			return err
		}
		if !row.AccountingFrozenAt.IsZero() {
			return errors.New("execution accounting is already frozen")
		}
		if err := settleAccounting(next, tracked, row, at, OutcomeCancelled, usage); err != nil {
			return err
		}
		row.AccountingFrozenAt = at
		changes, err = next.changes()
		if err != nil {
			return err
		}
		return writeRecordChangesTx(tx, changes, next.NextID, s.revision)
	})
	if err != nil {
		return err
	}
	s.revision++
	s.installLocked(next, changes)
	return nil
}
