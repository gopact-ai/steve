package work

import (
	"context"
	"errors"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/task"
)

// Budget charges executions to their task's budget: one row per attempt,
// reserved when the attempt is leased and settled from its durable record.
// The run is bounded by the time the task has left.
type Budget struct{ Tasks *task.Store }

func (b Budget) Reserve(taskID string) (int, time.Time, error) {
	tracked, err := b.Tasks.ReserveTurn(taskID)
	if err != nil {
		return 0, time.Time{}, err
	}
	return left(tracked)
}

func (b Budget) ReserveAttempt(record attempt.Record) (int, time.Time, error) {
	if record.Execution == nil {
		return 0, time.Time{}, errors.New("budget reservation needs the original execution token")
	}
	tracked, err := b.Tasks.ReserveAttempt(*record.Execution, record.ID, record.TurnID, record.Agent, record.Node, record.StartedAt)
	if err != nil {
		return 0, time.Time{}, err
	}
	return left(tracked)
}

func (b Budget) SettleAttempt(ctx context.Context, record attempt.Record, outcome task.Outcome) error {
	return b.Tasks.SettleAttempt(ctx, record.TaskID, record.ID, record.TurnID, record.EndedAt, outcome, attempt.StoppedUsage(record))
}

func left(t task.Task) (int, time.Time, error) {
	turns := max(0, t.Budget.MaxTurns-t.Budget.Turns)
	var deadline time.Time
	if t.Budget.MaxElapsed > 0 {
		deadline = time.Now().Add(t.Budget.MaxElapsed - t.Budget.Elapsed)
	}
	return turns, deadline, nil
}
