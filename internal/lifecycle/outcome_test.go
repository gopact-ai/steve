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
		// Only the error's identity counts: an agent's own HTTP call that
		// timed out is a failure of the run, not the run timing out.
		{"deadline text", errors.New(`Post "http://127.0.0.1/v1": ` + context.DeadlineExceeded.Error()), task.OutcomeError},
		{"cancel text", errors.New("node: " + context.Canceled.Error()), task.OutcomeError},
		{"failure", errors.New("exit status 1"), task.OutcomeError},
	} {
		if got := OutcomeOf(tc.err); got != tc.want {
			t.Errorf("%s: OutcomeOf(%v) = %s, want %s", tc.name, tc.err, got, tc.want)
		}
	}
}
