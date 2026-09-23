package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/task"
)

func TestOutcomeOfClassifiesHowARunEnded(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want task.Outcome
	}{
		{"success", nil, task.OutcomeOK},
		{"deadline", fmt.Errorf("prompt: %w", context.DeadlineExceeded), task.OutcomeTimeout},
		{"canceled context", fmt.Errorf("prompt: %w", context.Canceled), task.OutcomeCancelled},
		{"canceled turn", fmt.Errorf("prompt: %w", harness.ErrTurnCanceled), task.OutcomeCancelled},
		// A remote session's error arrives as text, without its identity.
		{"remote deadline", errors.New("node: " + context.DeadlineExceeded.Error()), task.OutcomeTimeout},
		{"remote cancel", errors.New("node: " + context.Canceled.Error()), task.OutcomeCancelled},
		{"failure", errors.New("exit status 1"), task.OutcomeError},
	} {
		if got := OutcomeOf(tc.err); got != tc.want {
			t.Errorf("%s: OutcomeOf(%v) = %s, want %s", tc.name, tc.err, got, tc.want)
		}
	}
}
