package turn

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/schedule"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
)

// closeRecorder remembers which native sessions were closed, so a test can
// tell a forgotten record from an ended agent process.
type closeRecorder struct {
	*fakeManager
	closed []string
}

func (m *closeRecorder) CloseSession(_ context.Context, _ harness.Placement, id string) error {
	m.closed = append(m.closed, id)
	return nil
}

func discardFixture(t *testing.T) (*Coordinator, *closeRecorder, *task.Store, *schedule.Store) {
	t.Helper()
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"worker": {Harness: "mock", Default: true}})
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	manager := &closeRecorder{fakeManager: &fakeManager{runners: map[string]*fakeRunner{"mock": {reply: "ok"}}}}
	c := newCoordinator(t, catalog, store, capability.NewAssembler(nil), manager, time.Minute)
	tasks, err := task.Open(filepath.Join(t.TempDir(), "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	c.SetTasks(tasks, "")
	schedules, err := schedule.Open(filepath.Join(t.TempDir(), "schedules.json"))
	if err != nil {
		t.Fatal(err)
	}
	c.SetSchedules(schedules)
	return c, manager, tasks, schedules
}

func TestDiscardConversationClosesSessionsAndDropsItsWork(t *testing.T) {
	c, manager, tasks, schedules := discardFixture(t)
	if err := c.store.SaveSession(state.Session{ConversationID: "console:one", AgentID: "worker", HarnessID: "mock", UpstreamID: "ns_1", Workspace: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	created, err := tasks.Create(task.Task{Goal: "ship it", Channel: "console:one", Member: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := schedules.Create(schedule.Job{ConversationID: "console:one", Member: "worker", Prompt: "nightly", Spec: schedule.Spec{Kind: schedule.KindEvery, Every: time.Hour, Text: "every hour"}}); err != nil {
		t.Fatal(err)
	}
	kept, err := schedules.Create(schedule.Job{ConversationID: "console:two", Member: "worker", Prompt: "weekly", Spec: schedule.Spec{Kind: schedule.KindEvery, Every: time.Hour, Text: "every hour"}})
	if err != nil {
		t.Fatal(err)
	}

	if err := c.DiscardConversation(context.Background(), "console:one"); err != nil {
		t.Fatalf("discard: %v", err)
	}

	if len(manager.closed) != 1 || manager.closed[0] != "ns_1" {
		t.Fatalf("closed sessions = %v; want [ns_1]", manager.closed)
	}
	if _, ok := tasks.Get(created.ID); ok {
		t.Fatalf("task %s survived its conversation", created.ID)
	}
	if jobs := schedules.List("console:one"); len(jobs) != 0 {
		t.Fatalf("schedules survived: %+v", jobs)
	}
	if jobs := schedules.List("console:two"); len(jobs) != 1 || jobs[0].ID != kept.ID {
		t.Fatalf("another conversation's schedule was dropped: %+v", jobs)
	}
	if sessions := c.store.Conversation("console:one").Sessions; len(sessions) != 0 {
		t.Fatalf("session records survived: %+v", sessions)
	}
}

func TestDiscardConversationRefusesWhileATurnRuns(t *testing.T) {
	c, _, tasks, _ := discardFixture(t)
	created, err := tasks.Create(task.Task{Goal: "ship it", Channel: "console:one", Member: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.cancels[sessionKey("console:one", "worker")] = &turnEntry{done: make(chan struct{})}
	c.mu.Unlock()

	if err := c.DiscardConversation(context.Background(), "console:one"); err == nil {
		t.Fatalf("discarded a conversation with a turn in flight")
	}
	if _, ok := tasks.Get(created.ID); !ok {
		t.Fatalf("refused discard still deleted task %s", created.ID)
	}
}
