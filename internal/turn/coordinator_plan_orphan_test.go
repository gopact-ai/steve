package turn

import (
	"errors"
	"testing"

	"github.com/gopact-ai/steve/internal/task"
)

// A background plan — an automatic conflict resolution, say — has no
// conversation, so recovery cannot reply to it and the task commands
// cannot reach it by id. If its run dies with the process, the only thing
// that can close it is the restart itself.
func TestARestartClosesBackgroundPlanTasksNothingElseCanReach(t *testing.T) {
	store, err := task.OpenLedger(testLedger(t))
	if err != nil {
		t.Fatal(err)
	}
	stranded, err := store.Create(task.Task{Goal: "resolve merge conflict abc in daily-works", Origin: "plan"})
	if err != nil {
		t.Fatal(err)
	}
	resuming, err := store.Create(task.Task{Goal: "still has a run", Origin: "plan"})
	if err != nil {
		t.Fatal(err)
	}
	owned, err := store.Create(task.Task{Goal: "a person asked for this", Origin: "plan", Channel: "console:c"})
	if err != nil {
		t.Fatal(err)
	}
	chat, err := store.Create(task.Task{Goal: "ordinary work", Channel: "console:c"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{stranded.ID, resuming.ID, owned.ID, chat.ID} {
		if _, err := store.Advance(id, task.StateRunning); err != nil {
			t.Fatal(err)
		}
	}

	c := New(nil, nil, nil, nil, 0)
	c.SetTasks(store, "hub")
	c.cancelOrphanedPlans(map[string]bool{resuming.ID: true})

	if got, _ := store.Get(stranded.ID); got.State != task.StateCancelled {
		t.Fatalf("stranded background plan left as %s", got.State)
	}
	for _, id := range []string{resuming.ID, owned.ID, chat.ID} {
		if got, _ := store.Get(id); got.State != task.StateRunning {
			t.Fatalf("task %s should have been left alone, got %s", id, got.State)
		}
	}
}

// A resolution that cannot run still opened a task. Left open it reads as
// work in flight for good, so the failure has to land on the record — and
// a person has to be able to settle it even though it is nobody's chat.
func TestAFailedBackgroundPlanIsRecordedAndCanBeSettled(t *testing.T) {
	store, err := task.OpenLedger(testLedger(t))
	if err != nil {
		t.Fatal(err)
	}
	tracked, err := store.Create(task.Task{Goal: "resolve merge conflict abc in daily-works", Origin: "plan"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(tracked.ID, task.StateRunning); err != nil {
		t.Fatal(err)
	}

	c := New(nil, nil, nil, nil, 0)
	c.SetTasks(store, "hub")
	c.closePlanTask(tracked.ID, errors.New("no agent could take it"))

	got, _ := store.Get(tracked.ID)
	if got.State != task.StateFailed {
		t.Fatalf("failed resolution left as %s", got.State)
	}
	cmds := c.commands()
	if _, found := cmds.taskTarget(Request{ConversationID: "console:someone-elses-chat"}, tracked.ID, taskIgnored); !found {
		t.Fatal("a task that belongs to no chat cannot be settled from any chat")
	}
	if _, err := store.Settle(tracked.ID, task.SettlementIgnored); err != nil {
		t.Fatal(err)
	}
}

// A plan whose last step closed the task already has the better answer.
// Closing it a second time used to log an error about moving from done to
// done on every successful resolution.
func TestClosingAPlanTaskLeavesAnAlreadyFinishedOneAlone(t *testing.T) {
	store, err := task.OpenLedger(testLedger(t))
	if err != nil {
		t.Fatal(err)
	}
	finished, err := store.Create(task.Task{Goal: "already closed by its last step", Origin: "plan"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(finished.ID, task.StateDone); err != nil {
		t.Fatal(err)
	}
	resumed, err := store.Create(task.Task{Goal: "resumed to the end", Origin: "plan"})
	if err != nil {
		t.Fatal(err)
	}

	c := New(nil, nil, nil, nil, 0)
	c.SetTasks(store, "hub")
	c.closePlanTask(finished.ID, errors.New("late failure"))
	c.closePlanTask(resumed.ID, nil)

	if got, _ := store.Get(finished.ID); got.State != task.StateDone {
		t.Fatalf("a finished plan task was reopened as %s", got.State)
	}
	if got, _ := store.Get(resumed.ID); got.State != task.StateDone {
		t.Fatalf("a resumed plan task was left as %s", got.State)
	}
}
