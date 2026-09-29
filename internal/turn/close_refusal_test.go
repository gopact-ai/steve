package turn

import (
	"errors"
	"fmt"
	"testing"

	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/task"
)

// A refused reset or project switch says why, in the terms of the check
// that refused it, and how to let the task go.
func TestCloseRefusalSaysWhy(t *testing.T) {
	c, _ := taskCoordinator(t, &fakeRunner{reply: "ok"})
	for _, tc := range []struct {
		name string
		err  error
		key  i18n.Key
	}{
		{"busy", fmt.Errorf("attempt att-1: %w", task.ErrCompleteBusy), i18n.TaskCloseBusy},
		{"delivery", fmt.Errorf("exchange e1: %w", task.ErrCompleteDelivery), i18n.TaskCloseDelivery},
		{"attention", fmt.Errorf("question q1: %w", task.ErrCompleteAttention), i18n.TaskCloseAttention},
		{"unverified", errors.New("ledger unavailable"), i18n.TaskCloseFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := c.closeRefusal("chat", "7", tc.err)
			if want := c.text.T(tc.key, "7", protocol.CommandTasks); got.Text != want {
				t.Fatalf("refusal for %v = %q, want %q", tc.err, got.Text, want)
			}
		})
	}
}
