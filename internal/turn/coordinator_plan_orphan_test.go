package turn

import (
	"errors"
	"testing"

	"github.com/gopact-ai/steve/internal/protocol"
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

	c := buildCoordinator(t, withDeps(func(d *Deps) { d.Tasks, d.Node = store, "hub" }))
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

// A resolution that cannot run still opened a task. Its failure must land
// on the record, and the authenticated Console owner can reconcile it without
// giving an arbitrary chat the authority to adopt background work.
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

	c := buildCoordinator(t, withDeps(func(d *Deps) { d.Tasks, d.Node, d.Identity.ConsoleOwner = store, "hub", "owner" }))
	c.closePlanTask(tracked.ID, errors.New("no agent could take it"))

	got, _ := store.Get(tracked.ID)
	if got.State != task.StateFailed {
		t.Fatalf("failed resolution left as %s", got.State)
	}
	cmds := c.commands()
	owner := Request{Source: Source{Channel: "console", ConversationID: "console:admin", ChatType: protocol.ChatP2P}, Actor: Actor{ID: "owner"}}
	if _, found := cmds.taskTarget(owner, tracked.ID, taskIgnored); !found {
		t.Fatal("the Console owner cannot reconcile an unaddressed failed plan")
	}
	for _, denied := range []Request{
		{Source: Source{Channel: "console", ConversationID: "console:guest", ChatType: protocol.ChatP2P}, Actor: Actor{ID: "guest"}},
		{Source: Source{Channel: "native", ConversationID: "console:opaque", ChatType: protocol.ChatP2P}, Actor: Actor{ID: "owner"}},
		{Source: Source{Channel: "console", ConversationID: "console:admin", ChatType: protocol.ChatGroup}, Actor: Actor{ID: "owner"}},
	} {
		if _, found := cmds.taskTarget(denied, tracked.ID, taskIgnored); found {
			t.Fatalf("unaddressed plan admitted arbitrary source: %+v", denied.Source)
		}
	}
	if _, err := cmds.tasksCmd(t.Context(), owner, "ignore "+tracked.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = store.Get(tracked.ID)
	if got.Settlement != task.SettlementIgnored || got.Channel != "" || got.Transport != "" {
		t.Fatal("owner reconciliation did not settle the plan without adopting its source")
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

	c := buildCoordinator(t, withDeps(func(d *Deps) { d.Tasks, d.Node = store, "hub" }))
	c.closePlanTask(finished.ID, errors.New("late failure"))
	c.closePlanTask(resumed.ID, nil)

	if got, _ := store.Get(finished.ID); got.State != task.StateDone {
		t.Fatalf("a finished plan task was reopened as %s", got.State)
	}
	if got, _ := store.Get(resumed.ID); got.State != task.StateDone {
		t.Fatalf("a resumed plan task was left as %s", got.State)
	}
}
