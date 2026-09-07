package agentexec

import (
	"context"
	"errors"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/task"
)

// AttemptBudget projects independently driven executions into their own task
// accounting rows. The ledger attempt remains the authoritative result.
type AttemptBudget interface {
	ReserveAttempt(attempt.Record) (int, time.Time, error)
	SettleAttempt(attempt.Record, task.Outcome) error
}

func ReserveBudget(budget Budget, record attempt.Record) (int, time.Time, error) {
	if exact, ok := budget.(AttemptBudget); ok {
		return exact.ReserveAttempt(record)
	}
	return budget.Reserve(record.TaskID)
}

func SettleBudget(budget Budget, record attempt.Record, cause error) error {
	exact, ok := budget.(AttemptBudget)
	if !ok {
		return nil
	}
	if !record.State.Terminal() || record.Unsettled {
		return errors.New("execution has no durable settlement for accounting")
	}
	outcome := task.OutcomeOK
	if record.State != attempt.Bound || cause != nil {
		outcome = task.OutcomeError
		if errors.Is(cause, context.DeadlineExceeded) {
			outcome = task.OutcomeTimeout
		} else if errors.Is(cause, context.Canceled) || errors.Is(cause, harness.ErrTurnCanceled) {
			outcome = task.OutcomeCancelled
		}
	}
	return exact.SettleAttempt(record, outcome)
}
