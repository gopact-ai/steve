package lifecycle

import (
	"context"
	"errors"

	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/task"
)

// OutcomeOf is how a run that returned err ended, for task accounting. Only
// the error's identity counts: a message that merely mentions a context
// error, such as an agent's own timed-out HTTP call, is a failure.
func OutcomeOf(err error) task.Outcome {
	switch {
	case err == nil:
		return task.OutcomeOK
	case errors.Is(err, context.DeadlineExceeded):
		return task.OutcomeTimeout
	case errors.Is(err, harness.ErrTurnCanceled) || errors.Is(err, context.Canceled):
		return task.OutcomeCancelled
	default:
		return task.OutcomeError
	}
}
