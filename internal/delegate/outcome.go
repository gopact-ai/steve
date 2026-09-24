package delegate

import (
	"context"
	"errors"
	"strings"

	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/task"
)

// remoteOutcomeOf is how a delegated child's run ended: by a local context
// error, or by the code the child's node gave its remote error. A failure
// read back from an attempt record ends as recorded; one recorded without
// an outcome — the ledger keeps only the message of a failed attempt — is
// classified by that message.
func remoteOutcomeOf(err error) task.Outcome {
	code := harness.RemoteErrorCode(err)
	var recorded *recorded
	if errors.As(err, &recorded) {
		if recorded.outcome != "" {
			return recorded.outcome
		}
		code = recordedCode(recorded.message)
	}
	switch {
	case err == nil:
		return task.OutcomeOK
	case errors.Is(err, context.DeadlineExceeded) || code == nodewire.SessionErrorDeadline:
		return task.OutcomeTimeout
	case errors.Is(err, harness.ErrTurnCanceled) || errors.Is(err, context.Canceled) || code == nodewire.SessionErrorCanceled:
		return task.OutcomeCancelled
	default:
		return task.OutcomeError
	}
}

// recordedCode classifies an attempt record's failure message, which
// carries a context error's text when one ended the run.
func recordedCode(message string) string {
	switch {
	case strings.Contains(message, context.DeadlineExceeded.Error()):
		return nodewire.SessionErrorDeadline
	case strings.Contains(message, context.Canceled.Error()):
		return nodewire.SessionErrorCanceled
	default:
		return nodewire.SessionErrorFailed
	}
}

// recorded is a failure read back from a child's attempt record, with the
// outcome that was classified when it was saved.
type recorded struct {
	message string
	outcome task.Outcome
}

func (e *recorded) Error() string { return e.message }

func recordedFailure(result agentmcp.DelegateResult, message string) error {
	return &recorded{message: message, outcome: result.Outcome}
}
