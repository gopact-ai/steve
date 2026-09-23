package delegate

import (
	"context"
	"errors"
	"strings"

	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/task"
)

// remoteOutcomeOf is how a delegated child's run ended. Its error may be
// relayed from a remote session as text, without its identity, so context
// errors are also recognized by message.
func remoteOutcomeOf(err error) task.Outcome {
	switch {
	case err == nil:
		return task.OutcomeOK
	case relayed(err, context.DeadlineExceeded):
		return task.OutcomeTimeout
	case errors.Is(err, harness.ErrTurnCanceled) || relayed(err, context.Canceled):
		return task.OutcomeCancelled
	default:
		return task.OutcomeError
	}
}

func relayed(err, target error) bool {
	return errors.Is(err, target) || strings.Contains(err.Error(), target.Error())
}
