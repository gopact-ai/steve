package turn

import (
	"errors"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/protocol"
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
		if _, err := c.StopRetainedTask(t.Context(), r.TaskID, wrong, true); err == nil {
			t.Fatal("stop with another identity accepted")
		}
		if tracked, _ := c.tasks.Get(r.TaskID); tracked.State == task.StateCancelled {
			t.Fatal("invalid stop revoked task")
		}
	}
	if _, err := c.StopRetainedTask(t.Context(), r.TaskID, req, true); !errors.Is(err, harness.ErrStopUnconfirmed) {
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
	if _, err := c.StopRetainedTask(t.Context(), r.TaskID, req, true); err != nil {
		t.Fatalf("confirmed stop still uncertain: %v", err)
	}
}

// Stopping the original execution of a paused task keeps it paused: the
// owner paused it to pick it back up later, and a stop that cancelled it
// would take that away.
func TestStopRetainedTaskKeepsAPausedTaskPaused(t *testing.T) {
	c, _, _, r, req := retainedChatFixture(t)
	if _, err := c.tasks.SetAside(r.TaskID, task.StatePaused); err != nil {
		t.Fatal(err)
	}
	if _, err := c.StopRetainedTask(t.Context(), r.TaskID, req, false); !errors.Is(err, harness.ErrStopUnconfirmed) {
		t.Fatalf("missing native stop receipt reported success: %v", err)
	}
	if tracked, _ := c.tasks.Get(r.TaskID); tracked.State != task.StatePaused {
		t.Fatalf("unconfirmed stop of a paused task left it %s, want paused", tracked.State)
	}
	if err := c.attempts.MarkUnsettled(t.Context(), r.ID, "test", harness.ErrStopUnconfirmed, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.attempts.ConfirmStopped(t.Context(), r.ID, "test", "test executor process exited"); err != nil {
		t.Fatal(err)
	}
	result, err := c.StopRetainedTask(t.Context(), r.TaskID, req, false)
	if want := c.text.T(i18n.TaskPaused, r.TaskID, protocol.CommandTasks); err != nil || !strings.HasPrefix(result.Text, want) {
		t.Fatalf("confirmed stop of a paused task = %q, %v; want it to say %q", result.Text, err, want)
	}
	if tracked, _ := c.tasks.Get(r.TaskID); tracked.State != task.StatePaused {
		t.Fatalf("confirmed stop of a paused task left it %s, want paused", tracked.State)
	}
}

// A retained execution is found with its task's durable state, so whoever
// recovers it can tell a task the owner set aside from one still running
// without anything kept in memory.
func TestRetainedChatsCarryTheTaskState(t *testing.T) {
	c, _, _, r, req := retainedChatFixture(t)
	for _, want := range []task.State{task.StateRunning, task.StatePaused, task.StateCancelled} {
		if want != task.StateRunning {
			if _, err := c.tasks.SetAside(r.TaskID, want); err != nil {
				t.Fatal(err)
			}
		}
		items, err := c.RetainedChatsFor(t.Context(), req.ConversationID, req.MessageID)
		if err != nil || len(items) != 1 || items[0].TaskID != r.TaskID || items[0].TaskState != want {
			t.Fatalf("retained chats with the task %s = %+v, %v; want its one execution carrying that state", want, items, err)
		}
	}
}

// A retained plan is found with its task's durable state too.
func TestRetainedPlansCarryTheTaskState(t *testing.T) {
	c, _, identity, _ := retainedPlanFixture(t)
	created, _ := c.tasks.Get(identity.TaskID)
	for _, want := range []task.State{created.State, task.StatePaused, task.StateCancelled} {
		if want != created.State {
			if _, err := c.tasks.SetAside(identity.TaskID, want); err != nil {
				t.Fatal(err)
			}
		}
		items, err := c.RetainedPlans(t.Context())
		if err != nil || len(items) != 1 || items[0].TaskID != identity.TaskID || items[0].TaskState != want {
			t.Fatalf("retained plans with the task %s = %+v, %v; want the plan carrying that state", want, items, err)
		}
	}
}

func TestOwnerStopCancelsAPausedRetainedTask(t *testing.T) {
	c, _, _, r, req := retainedChatFixture(t)
	if _, err := c.tasks.SetAside(r.TaskID, task.StatePaused); err != nil {
		t.Fatal(err)
	}
	if _, err := c.StopRetainedTask(t.Context(), r.TaskID, req, true); !errors.Is(err, harness.ErrStopUnconfirmed) {
		t.Fatalf("stop = %v", err)
	}
	if tracked, _ := c.tasks.Get(r.TaskID); tracked.State != task.StateCancelled {
		t.Fatalf("owner stop left task %s, want cancelled", tracked.State)
	}
}

func TestAutomaticStopCannotCancelARunningRetainedTask(t *testing.T) {
	c, _, _, r, req := retainedChatFixture(t)
	before, _ := c.tasks.Get(r.TaskID)
	if _, err := c.StopRetainedTask(t.Context(), r.TaskID, req, false); err == nil {
		t.Fatal("automatic check of a running task succeeded")
	}
	after, _ := c.tasks.Get(r.TaskID)
	if after.State != before.State || after.ExecutionEpoch != before.ExecutionEpoch {
		t.Fatalf("automatic check changed running task: before %+v, after %+v", before, after)
	}
}
