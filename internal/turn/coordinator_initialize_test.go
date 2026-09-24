package turn

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/datalevel"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
)

func TestInitializeConversationBindsWithoutTouchingSessionsOrTasks(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Default: true}, "other": {Harness: "codex"}})
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	manager := &selectorRuntime{fakeManager: &fakeManager{runners: map[string]*fakeRunner{}}}
	c := newCoordinator(t, catalog, store, capability.NewAssembler(nil), manager, time.Minute)
	c.SetIdentity("owner", nil)
	tasks, err := task.OpenLedger(taskBook(t))
	if err != nil {
		t.Fatal(err)
	}
	c.SetTasks(tasks, "")
	if err := c.InitializeConversation(t.Context(), "chat", "codex", "owner"); err != nil {
		t.Fatal(err)
	}
	if len(manager.opened) != 0 || len(manager.closed) != 0 || len(store.Conversation("chat").Sessions) != 0 || len(tasks.List("")) != 0 {
		t.Fatal("initialization admitted work")
	}
	binding, _, _ := c.projects.Binding(t.Context(), "chat")
	if err := store.SaveSession(state.Session{ConversationID: "chat", AgentID: "codex", HarnessID: "codex", UpstreamID: "ns_saved", ProjectID: "codex", ProjectVersion: binding.Version}); err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Create(task.Task{Channel: "chat", Member: "codex", ProjectID: "codex", Goal: "existing task"}); err != nil {
		t.Fatal(err)
	}
	beforeSession, beforeTasks := store.Conversation("chat"), tasks.List("")
	for _, projectID := range []string{"codex", "other"} {
		err := c.InitializeConversation(t.Context(), "chat", projectID, "owner")
		if (projectID == "codex") != (err == nil) {
			t.Fatalf("initialize %s = %v", projectID, err)
		}
	}
	if !reflect.DeepEqual(store.Conversation("chat"), beforeSession) || !reflect.DeepEqual(tasks.List(""), beforeTasks) || len(manager.closed) != 0 || len(manager.opened) != 0 {
		t.Fatal("retry or refused rebind changed existing work")
	}
	if got, _, _ := c.projects.Binding(t.Context(), "chat"); got != binding {
		t.Fatalf("binding changed: %+v", got)
	}
}

func TestInitializeConversationRequiresProjectReadAccess(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Default: true}})
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	c := newCoordinator(t, catalog, store, capability.NewAssembler(nil), &fakeManager{}, time.Minute)
	c.SetIdentity("owner", nil)
	if err := c.projects.Declare(t.Context(), []project.Project{{ID: "private", Level: datalevel.Restricted, Home: project.Home{Path: t.TempDir()}}, {ID: "readable", DefaultRole: project.RoleRead, Home: project.Home{Path: t.TempDir()}}}); err != nil {
		t.Fatal(err)
	}
	for _, projectID := range []string{"missing", "private"} {
		if err := c.InitializeConversation(t.Context(), "chat", projectID, "guest"); err == nil {
			t.Fatalf("accepted %s", projectID)
		}
		if _, ok, _ := c.projects.Binding(t.Context(), "chat"); ok {
			t.Fatal("refused initialization left binding")
		}
	}
	if err := c.InitializeConversation(t.Context(), "chat", "readable", "guest"); err != nil {
		t.Fatalf("read access should suffice: %v", err)
	}
}
