package lifecycle

import (
	"context"
	"errors"
	"strings"

	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/task"
)

// OutcomeOf is how a run that returned err ended, for task accounting. An
// error relayed from a remote session carries only its text, so the context
// errors are also recognized by message.
func OutcomeOf(err error) task.Outcome {
	switch {
	case err == nil:
		return task.OutcomeOK
	case isContextErr(err, context.DeadlineExceeded):
		return task.OutcomeTimeout
	case errors.Is(err, harness.ErrTurnCanceled) || isContextErr(err, context.Canceled):
		return task.OutcomeCancelled
	default:
		return task.OutcomeError
	}
}

func isContextErr(err, target error) bool {
	return errors.Is(err, target) || strings.Contains(err.Error(), target.Error())
}
