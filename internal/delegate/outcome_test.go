package delegate

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/task"
)

// A delegated child's error may be relayed from its remote session as
// text only, so its context errors are also recognized by message.
func TestRemoteOutcomeOfRecognizesRelayedContextErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want task.Outcome
	}{
		{"success", nil, task.OutcomeOK},
		{"deadline", fmt.Errorf("prompt: %w", context.DeadlineExceeded), task.OutcomeTimeout},
		{"canceled turn", fmt.Errorf("prompt: %w", harness.ErrTurnCanceled), task.OutcomeCancelled},
		{"remote deadline", errors.New("node: " + context.DeadlineExceeded.Error()), task.OutcomeTimeout},
		{"remote cancel", errors.New("node: " + context.Canceled.Error()), task.OutcomeCancelled},
		{"failure", errors.New("exit status 1"), task.OutcomeError},
	} {
		if got := remoteOutcomeOf(tc.err); got != tc.want {
			t.Errorf("%s: remoteOutcomeOf(%v) = %s, want %s", tc.name, tc.err, got, tc.want)
		}
	}
}
