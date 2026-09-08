package turn

import (
	"context"
	"errors"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/task"
)

func (c *Coordinator) settleRetained(parent context.Context, record attempt.Record, result Result, runErr error, spent *turnSpend) (error, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 30*time.Second)
	defer cancel()
	if record.Execution != nil && c.tasks != nil && errors.Is(c.tasks.CheckExecution(*record.Execution), task.ErrExecutionStopped) {
		runErr = harness.ErrTurnCanceled
	}
	if err := c.attempts.MarkSessionSettled(ctx, record.ID, "retained-settlement"); err != nil {
		return runErr, err
	}
	if err := c.closeAttempt(ctx, record.ID, result, runErr, spent, nil); err != nil {
		return runErr, err
	}
	return runErr, nil
}
