package agentexec

import (
	"context"
	"errors"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/lifecycle"
	"github.com/gopact-ai/steve/internal/task"
)

// AttemptBudget projects independently driven executions into their own task
// accounting rows. The ledger attempt remains the authoritative result.
type AttemptBudget interface {
	ReserveAttempt(attempt.Record) (int, time.Time, error)
	SettleAttempt(context.Context, attempt.Record, task.Outcome) error
}

func ReserveBudget(budget Budget, record attempt.Record) (int, time.Time, error) {
	if exact, ok := budget.(AttemptBudget); ok {
		return exact.ReserveAttempt(record)
	}
	return budget.Reserve(record.TaskID)
}

func SettleBudget(ctx context.Context, budget Budget, record attempt.Record, cause error) error {
	exact, ok := budget.(AttemptBudget)
	if !ok {
		return nil
	}
	if !record.State.Terminal() || record.Unsettled {
		return errors.New("execution has no durable settlement for accounting")
	}
	outcome := lifecycle.OutcomeOf(cause)
	if outcome == task.OutcomeOK && record.State != attempt.Bound {
		outcome = task.OutcomeError
	}
	return exact.SettleAttempt(ctx, record, outcome)
}
