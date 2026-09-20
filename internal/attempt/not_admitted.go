package attempt

import (
	"context"
	"database/sql"
	"errors"

	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

// UnadmittedTurn returns positive ended-input evidence only when the complete
// attempt identity index in that same committed snapshot has no admission.
// Unlike RetainedChats it excludes no execution phase, kind or session type.
func (s *Service) UnadmittedTurn(ctx context.Context, address channel.Address) (task.Task, bool, error) {
	turnID := address.Message
	var tracked task.Task
	confirmed := false
	err := s.l.Read(ctx, func(tx *ledger.ReadTx) error {
		var row task.Attempt
		var found bool
		var err error
		tracked, row, found, err = task.ReadTurnAccountingTx(tx, turnID)
		if err != nil {
			return err
		}
		legacy := !found
		if legacy {
			tracked, found, err = task.ReadCancelledTurnTx(tx, address)
			if err != nil || !found || len(tracked.Attempts) == 0 {
				return err
			}
			row = tracked.Attempts[len(tracked.Attempts)-1]
			if row.TurnID != "" || row.ExecutionEpoch >= tracked.ExecutionEpoch {
				return nil
			}
		}
		sameEpoch := row.ExecutionEpoch == tracked.ExecutionEpoch
		revoked := tracked.State == task.StateCancelled && row.ExecutionEpoch < tracked.ExecutionEpoch
		if !endedWithoutExecution(row, legacy) || (!sameEpoch && !revoked) || row.Member != tracked.Member {
			return nil
		}
		if err := checkIdentityRows(tx); err != nil {
			return err
		}
		_, err = scanIdentityRecord(tx.QueryRow(turnIdentitySQL, turnID))
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		// An admitted record with no input identity could be this input.
		// It cannot disappear behind an otherwise exact negative lookup.
		var unidentified string
		err = tx.QueryRow(`SELECT id FROM operations INDEXED BY operations_attempt_task
			WHERE kind='attempt' AND `+identityTask+`=? AND `+identityTurn+`='' LIMIT 1`, tracked.ID).Scan(&unidentified)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if legacy {
			safe, err := settledLegacyHistoryTx(tx, tracked, turnID)
			if err != nil || !safe {
				return err
			}
		}
		confirmed = true
		return nil
	})
	return tracked, confirmed && err == nil, err
}

func endedWithoutExecution(row task.Attempt, cancelledLegacy bool) bool {
	if row.Independent || row.Open() || row.StartedAt.IsZero() || row.EndedAt.Before(row.StartedAt) ||
		row.ExecutionID != "" || row.ExecutionEpoch == 0 || row.Member == "" || row.Session != "" {
		return false
	}
	switch row.Outcome {
	case task.OutcomeError, task.OutcomeCancelled, task.OutcomeTimeout:
		return true
	case task.OutcomeInterrupted:
		// This outcome alone never acknowledges handler completion. Only
		// explicit cancellation plus exhaustive legacy reconciliation can
		// prove the old input has no admitted execution and cannot gain one.
		return cancelledLegacy
	default:
		// Startup interruption is not an ended-handler receipt.
		return false
	}
}

// Every historical admission must have a distinct, known input and explicit
// settlement, and match its exact accounting row. The cancelled epoch fences
// delayed admission; neither it nor a missing retained session proves that a
// previously admitted process stopped.
func settledLegacyHistoryTx(tx *ledger.ReadTx, tracked task.Task, turnID string) (bool, error) {
	accounted := map[string]task.Attempt{}
	turns := map[string]bool{}
	for _, row := range tracked.Attempts[:len(tracked.Attempts)-1] {
		if row.Independent || row.Open() || row.StartedAt.IsZero() || row.EndedAt.Before(row.StartedAt) ||
			row.ExecutionID == "" || row.TurnID == "" || row.TurnID == turnID || turns[row.TurnID] ||
			row.ExecutionEpoch == 0 || row.ExecutionEpoch >= tracked.ExecutionEpoch || row.Member == "" {
			return false, nil
		}
		if _, duplicate := accounted[row.ExecutionID]; duplicate {
			return false, nil
		}
		accounted[row.ExecutionID], turns[row.TurnID] = row, true
	}
	rows, err := tx.Query(taskIdentitySQL, tracked.ID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		record, err := scanIdentityRecord(rows)
		if err != nil {
			return false, err
		}
		row, found := accounted[record.ID]
		if !found || record.Kind != KindChat || record.TurnID != row.TurnID || record.Agent != row.Member ||
			record.Project != tracked.ProjectID || record.Execution == nil || record.Execution.TaskID != tracked.ID ||
			record.Execution.Epoch != row.ExecutionEpoch || record.Unsettled || record.SessionSettled == nil || !*record.SessionSettled ||
			record.StartedAt.IsZero() || record.EndedAt.IsZero() || record.EndedAt.Before(record.StartedAt) {
			return false, nil
		}
		switch record.State {
		case Bound, Failed, Expired, BindConflict, Superseded:
		default:
			return false, nil
		}
		delete(accounted, record.ID)
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return len(accounted) == 0, nil
}
