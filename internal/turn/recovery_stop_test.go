package turn

import (
	"context"
	"errors"
	"github.com/gopact-ai/steve/internal/execution"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/task"
)

func TestStopRetainedTaskRequiresPhysicalSettlementAndOriginalIdentity(t *testing.T) {
	c, _, _, r, req := retainedChatFixture(t)
	for _, change := range []func(*Request){
		func(r *Request) { r.Source.MessageID = "web-other" },
		func(r *Request) { r.Source.ConversationID = "console:other" },
		func(r *Request) { r.Actor.ID = "other" },
		func(r *Request) { r.Admission.ExpectedProject = "other" },
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
		items, err := c.RetainedChatsFor(t.Context(), req.Source.ConversationID, req.Source.MessageID)
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
	result, err := c.StopRetainedTask(t.Context(), r.TaskID, req, true)
	if !errors.Is(err, harness.ErrStopUnconfirmed) {
		t.Fatalf("stop = %v", err)
	}
	if !strings.HasPrefix(result.Text, c.text.T(i18n.TaskCancelled, r.TaskID)) {
		t.Fatalf("explicit stop did not say the task was cancelled: %q", result.Text)
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

func TestAutomaticStopCheckDoesNotRevokeTheTaskAgain(t *testing.T) {
	c, _, _, r, req := retainedChatFixture(t)
	if _, err := c.tasks.SetAside(r.TaskID, task.StatePaused); err != nil {
		t.Fatal(err)
	}
	before, _ := c.tasks.Get(r.TaskID)
	_, _ = c.StopRetainedTask(t.Context(), r.TaskID, req, false)
	after, _ := c.tasks.Get(r.TaskID)
	if after.ExecutionEpoch != before.ExecutionEpoch {
		t.Fatalf("checking the stop revoked the task again: epoch %d -> %d", before.ExecutionEpoch, after.ExecutionEpoch)
	}
}

func TestStopSnapshotCannotStopAResumedExecution(t *testing.T) {
	c, _, _, record, _ := retainedChatFixture(t)
	old, err := c.executions.Begin(t.Context(), execution.Key{TaskID: record.TaskID, AttemptID: record.ID, InstanceID: "old"})
	if err != nil {
		t.Fatal(err)
	}
	var oldStops, newStops atomic.Int32
	if err := execution.RegisterStopHandler(old.Context(), "old", func(context.Context) error { oldStops.Add(1); return nil }); err != nil {
		t.Fatal(err)
	}
	old.Finish(harness.ErrStopUnconfirmed)
	if _, err := c.tasks.SetAside(record.TaskID, task.StatePaused); err != nil {
		t.Fatal(err)
	}
	captured := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		tree, err := c.tasks.Tree(record.TaskID)
		if err != nil {
			done <- err
			close(captured)
			return
		}
		close(captured)
		<-release
		_, err = c.checkRetainedStopSnapshot(t.Context(), tree)
		done <- err
	}()
	<-captured
	if _, err := c.tasks.Advance(record.TaskID, task.StateRunning); err != nil {
		close(release)
		<-done
		t.Fatal(err)
	}
	newer, err := c.executions.Begin(t.Context(), execution.Key{TaskID: record.TaskID, AttemptID: "new", InstanceID: "new"})
	if err != nil {
		close(release)
		<-done
		t.Fatal(err)
	}
	defer newer.Finish(nil)
	if err := execution.RegisterStopHandler(newer.Context(), "new", func(context.Context) error { newStops.Add(1); return nil }); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("snapshot check waited for the new execution")
		newer.Finish(nil)
		<-done
	}
	if newStops.Load() != 0 || newer.Context().Err() != nil {
		t.Fatal("old stop check reached the resumed execution")
	}
	if oldStops.Load() != 1 {
		t.Fatalf("old execution was not checked: %d", oldStops.Load())
	}
}

func TestReplayedCancelIntentDoesNotRevokeAgain(t *testing.T) {
	c, _, _, record, req := retainedChatFixture(t)
	if _, err := c.tasks.SetAside(record.TaskID, task.StateCancelled); err != nil {
		t.Fatal(err)
	}
	before, _ := c.tasks.Get(record.TaskID)
	_, _ = c.StopRetainedTask(t.Context(), record.TaskID, req, true)
	after, _ := c.tasks.Get(record.TaskID)
	if after.ExecutionEpoch != before.ExecutionEpoch {
		t.Fatalf("replayed cancellation advanced epoch %d -> %d", before.ExecutionEpoch, after.ExecutionEpoch)
	}
}

func TestStopSnapshotRefusesARunningDescendant(t *testing.T) {
	c, _, _, record, _ := retainedChatFixture(t)
	tree := []task.Task{{ID: record.TaskID, State: task.StatePaused, ExecutionEpoch: 2}, {ID: "child", Parent: record.TaskID, State: task.StateRunning, ExecutionEpoch: 3}}
	if _, err := c.checkRetainedStopSnapshot(t.Context(), tree); err == nil || !strings.Contains(err.Error(), "no longer waiting") {
		t.Fatalf("running descendant was not refused before stopping: %v", err)
	}
}
