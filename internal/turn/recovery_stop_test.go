package turn

import (
	"errors"
	"testing"

	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/task"
)

func TestStopRetainedTaskRequiresPhysicalSettlementAndOriginalIdentity(t *testing.T) {
	c, _, _, r, req := retainedChatFixture(t)
	for _, change := range []func(*Request){
		func(r *Request) { r.MessageID = "web-other" },
		func(r *Request) { r.ConversationID = "console:other" },
		func(r *Request) { r.SenderOpenID = "other" },
		func(r *Request) { r.ExpectedProject = "other" },
	} {
		wrong := req
		change(&wrong)
		if _, err := c.StopRetainedTask(t.Context(), r.TaskID, wrong); err == nil {
			t.Fatal("stop with another identity accepted")
		}
		if tracked, _ := c.tasks.Get(r.TaskID); tracked.State == task.StateCancelled {
			t.Fatal("invalid stop revoked task")
		}
	}
	if _, err := c.StopRetainedTask(t.Context(), r.TaskID, req); !errors.Is(err, harness.ErrStopUnconfirmed) {
		t.Fatalf("missing native stop receipt reported success: %v", err)
	}
	if tracked, _ := c.tasks.Get(r.TaskID); tracked.State != task.StateCancelled {
		t.Fatal("stop did not revoke original task")
	}
	if err := c.attempts.MarkUnsettled(t.Context(), r.ID, "test", harness.ErrStopUnconfirmed, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.attempts.ConfirmStopped(t.Context(), r.ID, "test", "test executor process exited"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.StopRetainedTask(t.Context(), r.TaskID, req); err != nil {
		t.Fatalf("confirmed stop still uncertain: %v", err)
	}
}
