package turn

import (
	"testing"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

func TestContinuationCannotCreateAnotherTaskOrResumeAPausedOne(t *testing.T) {
	for _, state := range []task.State{task.StatePaused, task.StateCancelled, task.StateDone, task.StateFailed} {
		t.Run(string(state), func(t *testing.T) {
			store, err := task.OpenLedger(testLedger(t))
			if err != nil {
				t.Fatal(err)
			}
			parent, err := store.Create(task.Task{Goal: "parent", Member: "worker", Channel: "console:c"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Advance(parent.ID, state); err != nil {
				t.Fatal(err)
			}
			c := newCore(nil, nil, nil, nil, 0)
			c.SetTasks(store, "hub")
			req := Request{ConversationID: parent.Channel, ExpectedTask: parent.ID}
			_, err = c.beginTask(req, agent.Agent{ID: "worker"}, "child done", project.Binding{}, "/work")
			if err == nil {
				t.Fatal("stale continuation was accepted")
			}
			if len(store.List("")) != 1 {
				t.Fatal("continuation created another task")
			}
			got, _ := store.Get(parent.ID)
			if got.State != state || len(got.Attempts) != 0 {
				t.Fatalf("continuation changed stopped task: %+v", got)
			}
		})
	}
}

func TestContinuationPreservesScheduledParentLineage(t *testing.T) {
	store, err := task.OpenLedger(testLedger(t))
	if err != nil {
		t.Fatal(err)
	}
	parent, err := store.Create(task.Task{Goal: "nightly", Member: "worker", Channel: "console:c", Origin: "schedule:nightly"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(parent.ID, task.StateRunning); err != nil {
		t.Fatal(err)
	}
	c := newCore(nil, nil, nil, nil, 0)
	c.SetTasks(store, "hub")
	id, err := c.beginTask(Request{ConversationID: parent.Channel, ExpectedTask: parent.ID}, agent.Agent{ID: "worker"}, "child done", project.Binding{}, "/work")
	if err != nil || id != parent.ID {
		t.Fatalf("lost scheduled lineage: id=%s err=%v", id, err)
	}
}

// testLedger opens a ledger that lives as long as the test.
func testLedger(t *testing.T) *ledger.Ledger {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = book.Close() })
	return book
}
