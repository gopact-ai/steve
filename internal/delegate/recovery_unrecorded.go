package delegate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/task"
)

// unrecordedAnswer is what the parent is told about a child that was never
// admitted: there was no execution, so there is nothing to resume from.
const unrecordedAnswer = "子任务在开始执行前中断，没有执行记录。如果仍需要这项工作，请重新委派。"

// inherited is the accounting rows open when the service was created:
// opened by an earlier process, never by this one. A row this process
// opens is its own run's to close, however that run ends.
type inherited struct {
	mu   sync.Mutex
	rows map[string]bool
}

// inheritedKey names one row for good: its task, the epoch it was opened
// under, and its position in the task's history.
func inheritedKey(candidate task.RecoveryCandidate) string {
	return fmt.Sprintf("%s/%d/%d", candidate.Task.ID, candidate.Attempt.ExecutionEpoch, candidate.Index)
}

func inheritedRows(tasks *task.Store) map[string]bool {
	rows := map[string]bool{}
	if tasks == nil {
		return rows
	}
	for _, candidate := range tasks.OpenPrimaryAccounting() {
		if candidate.Task.Delegated() {
			rows[inheritedKey(candidate)] = true
		}
	}
	return rows
}

// settleUnrecordedChildren closes the inherited rows of delegated children
// whose attempt never reached the ledger. run opens the child's row before
// lifecycle.Run admits its attempt; a hub that dies in between leaves a row
// no attempt record will ever settle, and the recovery that walks the
// records never sees it — while the open row keeps the whole tree from
// being completed. Such a row is settled as the interruption it is, and
// the child ends like one whose run failed, so its parent is told. A row
// whose record exists is that record's recovery to settle, however long it
// takes; one opened since this process started is its own run's.
func (s *Service) settleUnrecordedChildren(ctx context.Context) error {
	if !s.inherited.mu.TryLock() {
		return nil
	}
	defer s.inherited.mu.Unlock()
	for _, candidate := range s.tasks.OpenPrimaryAccounting() {
		if err := ctx.Err(); err != nil {
			return err
		}
		key := inheritedKey(candidate)
		if !s.inherited.rows[key] {
			continue
		}
		tracked, row := candidate.Task, candidate.Attempt
		if row.ExecutionID != "" {
			_, err := s.attempts.Get(ctx, row.ExecutionID)
			if err == nil {
				continue
			}
			if !errors.Is(err, attempt.ErrNotFound) {
				slog.Warn(fmt.Sprintf("delegate: task #%s accounting row not checked: %v", tracked.ID, err), "task", tracked.ID, "parent", tracked.Parent, "attempt", row.ExecutionID)
				continue
			}
		}
		if err := s.settleUnrecorded(tracked, row); err != nil {
			slog.Error(fmt.Sprintf("delegate: settle unrecorded task #%s: %v", tracked.ID, err), "task", tracked.ID, "parent", tracked.Parent, "attempt", row.ExecutionID, "node", tracked.Node)
			continue
		}
		delete(s.inherited.rows, key)
	}
	return nil
}

// settleUnrecorded closes the row, and ends the child unless its ending was
// already decided: a run that failed after opening the row recorded its
// own result, an owner who paused or cancelled the child had the last
// word. A resumed child opened its turn past the row the stop revoked
// (Begin closes such a row as never run); when that turn's own row is the
// one left here, it got no further, and the child is told the same way as
// any other failure; a turn still queued opens its row from failed. No
// time is charged for a row that never ran.
func (s *Service) settleUnrecorded(tracked task.Task, row task.Attempt) error {
	if row.ExecutionID == "" {
		if _, err := s.tasks.FinishUnstarted(tracked.ID, task.OutcomeInterrupted); err != nil {
			return err
		}
	} else if err := s.tasks.SettleAttempt(tracked.ID, row.ExecutionID, row.TurnID, row.StartedAt, task.OutcomeInterrupted, task.RecoveryUsage{}); err != nil {
		return err
	}
	slog.Warn(fmt.Sprintf("delegate: task #%s interrupted before its attempt was admitted; accounting row closed", tracked.ID), "task", tracked.ID, "parent", tracked.Parent, "attempt", row.ExecutionID, "node", tracked.Node, "state", tracked.State)
	if tracked.State != task.StateRunning {
		return nil
	}
	// Under the epoch the child was listed with: a stop that lands
	// meanwhile bumps it, and the move is refused rather than overriding
	// the user.
	if _, err := s.tasks.AdvanceExecution(task.ExecutionToken{TaskID: tracked.ID, Epoch: tracked.ExecutionEpoch}, task.StateFailed); err != nil {
		return err
	}
	if tracked.Result != nil {
		return nil
	}
	return s.tasks.SetResult(tracked.ID, task.Result{Outcome: task.OutcomeInterrupted, Answer: unrecordedAnswer, Attempt: row.ExecutionID})
}
