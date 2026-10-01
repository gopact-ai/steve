package turn

import (
	"sync"
	"testing"

	"github.com/gopact-ai/steve/internal/task"
)

func TestForceStopConfirmationRejectsAChangedOrActiveRevision(t *testing.T) {
	for _, mode := range []string{"changed", "active"} {
		t.Run(mode, func(t *testing.T) {
			c, tasks, r, _ := forceStopControlFixture(t)
			child, err := tasks.Create(task.Task{Parent: r.TaskID, Channel: "console:original", Member: "child"})
			if err != nil {
				t.Fatal(err)
			}
			control := NewForceStopControl(c)
			if err := control.ForceStopAttempt(t.Context(), r.ID, "owner", 0); err != nil {
				t.Fatal(err)
			}
			before, _ := c.attempts.Get(t.Context(), r.ID)
			parentBefore, _ := tasks.Get(r.TaskID)
			childBefore, _ := tasks.Get(child.ID)
			expected := uint64(0)
			if mode == "active" {
				expected = before.ForceStop.Revision
			}
			if err := control.ForceStopAttempt(t.Context(), r.ID, "owner", expected); err == nil {
				t.Fatal("stale or active confirmation started another force-stop revision")
			}
			after, _ := c.attempts.Get(t.Context(), r.ID)
			parentAfter, _ := tasks.Get(r.TaskID)
			childAfter, _ := tasks.Get(child.ID)
			if after.Revision != before.Revision || after.ForceStop.Revision != before.ForceStop.Revision || parentAfter.ExecutionEpoch != parentBefore.ExecutionEpoch || childAfter.ExecutionEpoch != childBefore.ExecutionEpoch {
				t.Fatal("refused confirmation changed the task tree or execution")
			}
		})
	}
}

func TestForceStopExhaustedRevisionNeedsAFreshConfirmation(t *testing.T) {
	c, tasks, r, _ := forceStopControlFixture(t)
	control := NewForceStopControl(c)
	if err := control.ForceStopAttempt(t.Context(), r.ID, "owner", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := c.attempts.RecordForceStopResult(t.Context(), r.ID, 1, true, "stop_unproven"); err != nil {
		t.Fatal(err)
	}
	before, _ := tasks.Get(r.TaskID)
	if err := control.ForceStopAttempt(t.Context(), r.ID, "owner", 0); err == nil {
		t.Fatal("a confirmation from before the last operation replayed it")
	}
	if err := control.ForceStopAttempt(t.Context(), r.ID, "owner", 1); err != nil {
		t.Fatalf("fresh confirmation could not retry exhaustion: %v", err)
	}
	after, _ := tasks.Get(r.TaskID)
	current, _ := c.attempts.Get(t.Context(), r.ID)
	if current.ForceStop.Revision != 2 || current.ForceStop.Level != "kill" || before.ExecutionEpoch != after.ExecutionEpoch {
		t.Fatal("explicit exhausted retry lost its revision or revoked the task again")
	}
}

func TestConcurrentForceStopConfirmationsHaveOneAcceptedRevision(t *testing.T) {
	c, _, r, _ := forceStopControlFixture(t)
	control := NewForceStopControl(c)
	var wg sync.WaitGroup
	ready := make(chan struct{})
	outcomes := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-ready
			outcomes <- control.ForceStopAttempt(t.Context(), r.ID, "owner", 0)
		}()
	}
	close(ready)
	wg.Wait()
	accepted := 0
	for range 2 {
		if <-outcomes == nil {
			accepted++
		}
	}
	current, _ := c.attempts.Get(t.Context(), r.ID)
	if accepted != 1 || current.ForceStop.Revision != 1 {
		t.Fatalf("two tabs both advanced the operation: accepted=%d revision=%d", accepted, current.ForceStop.Revision)
	}
}

func TestRetainedChatCarriesTheForceStopRevision(t *testing.T) {
	c, _, _, r, req := retainedChatFixture(t)
	if _, err := c.tasks.SetAside(r.TaskID, task.StateCancelled); err != nil {
		t.Fatal(err)
	}
	if _, err := c.attempts.RequestForceStop(t.Context(), r.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	items, err := c.RetainedChatsFor(t.Context(), req.ConversationID, req.MessageID)
	if err != nil || len(items) != 1 || items[0].ForceStopRevision != 1 {
		t.Fatalf("chat confirmation lost current revision: %+v %v", items, err)
	}
}

func TestRetainedPlanCarriesTheForceStopRevision(t *testing.T) {
	c, _, identity, _ := retainedPlanFixture(t)
	if _, err := c.tasks.SetAside(identity.TaskID, task.StateCancelled); err != nil {
		t.Fatal(err)
	}
	if _, err := c.attempts.RequestForceStop(t.Context(), identity.AttemptID, "owner"); err != nil {
		t.Fatal(err)
	}
	items, err := c.RetainedPlans(t.Context())
	if err != nil || len(items) != 1 || items[0].ForceStopRevision != 1 {
		t.Fatalf("plan confirmation lost current revision: %+v %v", items, err)
	}
}
