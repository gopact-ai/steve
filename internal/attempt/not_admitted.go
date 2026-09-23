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
// Unlike RetainedChatsFor it excludes no execution phase, kind or session type.
func (s *Service) UnadmittedTurn(ctx context.Context, address channel.Address) (task.Task, bool, error) {
	turnID := address.Message
	var tracked task.Task
	confirmed := false
	err := s.l.Read(ctx, func(tx *ledger.ReadTx) error {
		var row task.Attempt
		var found bool
		var err error
		tracked, row, found, err = task.ReadTurnAccountingTx(tx, turnID)
		if err != nil || !found {
			return err
		}
		sameEpoch := row.ExecutionEpoch == tracked.ExecutionEpoch
		revoked := tracked.State == task.StateCancelled && row.ExecutionEpoch < tracked.ExecutionEpoch
		if !endedWithoutExecution(row) || (!sameEpoch && !revoked) || row.Member != tracked.Member {
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
		confirmed = true
		return nil
	})
	return tracked, confirmed && err == nil, err
}

func endedWithoutExecution(row task.Attempt) bool {
	if row.Independent || row.Open() || row.StartedAt.IsZero() || row.EndedAt.Before(row.StartedAt) ||
		row.ExecutionID != "" || row.ExecutionEpoch == 0 || row.Member == "" || row.Session != "" {
		return false
	}
	switch row.Outcome {
	case task.OutcomeError, task.OutcomeCancelled, task.OutcomeTimeout:
		return true
	default:
		// Startup interruption is not an ended-handler receipt.
		return false
	}
}
