package delegate

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/task"
)

type codedRemoteError struct{ code string }

func (e codedRemoteError) Error() string           { return "remote prompt failed" }
func (e codedRemoteError) RemoteErrorCode() string { return e.code }

// A delegated child's remote error is classified by the code its node
// reported, never by what its message happens to say.
func TestRemoteOutcomeOfClassifiesByErrorIdentity(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want task.Outcome
	}{
		{"success", nil, task.OutcomeOK},
		{"deadline", fmt.Errorf("prompt: %w", context.DeadlineExceeded), task.OutcomeTimeout},
		{"canceled turn", fmt.Errorf("prompt: %w", harness.ErrTurnCanceled), task.OutcomeCancelled},
		{"canceled", fmt.Errorf("prompt: %w", context.Canceled), task.OutcomeCancelled},
		{"remote deadline", fmt.Errorf("node: %w", codedRemoteError{nodewire.SessionErrorDeadline}), task.OutcomeTimeout},
		{"remote cancel", fmt.Errorf("node: %w", codedRemoteError{nodewire.SessionErrorCanceled}), task.OutcomeCancelled},
		{"remote failure", codedRemoteError{nodewire.SessionErrorFailed}, task.OutcomeError},
		{"message that only mentions a deadline", errors.New("tool said: " + context.DeadlineExceeded.Error()), task.OutcomeError},
		{"message that only mentions a cancel", errors.New("tool said: " + context.Canceled.Error()), task.OutcomeError},
		{"failure", errors.New("exit status 1"), task.OutcomeError},
	} {
		if got := remoteOutcomeOf(tc.err); got != tc.want {
			t.Errorf("%s: remoteOutcomeOf(%v) = %s, want %s", tc.name, tc.err, got, tc.want)
		}
	}
}

// A child's failure recovered from its attempt record ends as the record
// says it did, whatever its saved message reads. A record saved without an
// outcome is classified by its message, as a node that sends no error code
// is.
func TestRecordedFailureEndsAsItsRecordSays(t *testing.T) {
	for _, tc := range []struct {
		name    string
		result  agentmcp.DelegateResult
		message string
		want    task.Outcome
	}{
		{"timeout", agentmcp.DelegateResult{Outcome: task.OutcomeTimeout}, "prompt timed out", task.OutcomeTimeout},
		{"cancelled", agentmcp.DelegateResult{Outcome: task.OutcomeCancelled}, "stopped", task.OutcomeCancelled},
		{"unrecorded deadline", agentmcp.DelegateResult{}, "prompt: " + context.DeadlineExceeded.Error(), task.OutcomeTimeout},
		{"unrecorded cancel", agentmcp.DelegateResult{}, "prompt: " + context.Canceled.Error(), task.OutcomeCancelled},
		{"unrecorded failure", agentmcp.DelegateResult{}, "exit status 1", task.OutcomeError},
	} {
		if got := remoteOutcomeOf(recordedFailure(tc.result, tc.message)); got != tc.want {
			t.Errorf("%s: outcome = %s, want %s", tc.name, got, tc.want)
		}
	}
}
