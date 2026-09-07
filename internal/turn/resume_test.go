package turn

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
)

func TestBeginTaskPersistsAnchor(t *testing.T) {
	runner := &fakeRunner{reply: "ok"}
	coordinator, tasks := taskCoordinator(t, runner)
	if _, err := coordinator.Handle(t.Context(), Request{
		ConversationID: "chat", Input: "干活", MessageID: "om_1", ChatID: "oc_1",
		ChatType: protocol.ChatGroup, Mentioned: true, CardID: "om_card_1",
	}); err != nil {
		t.Fatal(err)
	}
	tracked, ok := tasks.Active("chat", "codex", "")
	if !ok {
		t.Fatal("no active task")
	}
	if tracked.AnchorMessage != "om_1" || tracked.ChatID != "oc_1" || tracked.ChatType != "group" {
		t.Fatalf("anchor not persisted: %+v", tracked)
	}
	if tracked.OpenCard != "om_card_1" {
		t.Fatalf("open card not journaled: %+v", tracked)
	}
	// The next turn refreshes the anchor to the newest exchange.
	if _, err := coordinator.Handle(t.Context(), Request{
		ConversationID: "chat", Input: "接着", MessageID: "om_2", ChatID: "oc_1",
		ChatType: protocol.ChatGroup, Mentioned: true,
	}); err != nil {
		t.Fatal(err)
	}
	tracked, _ = tasks.Active("chat", "codex", "")
	if tracked.AnchorMessage != "om_2" {
		t.Fatalf("anchor not refreshed: %+v", tracked)
	}
}

func TestReviveSessionClearsTaintSoTheTurnRuns(t *testing.T) {
	workspace := t.TempDir()
	catalog, _ := agent.NewCatalog(map[string]agent.Config{
		"codex": {Harness: "codex", Default: true},
	})
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	// The saved hash must match what the next assembly produces, exactly as
	// it would for a session the same gateway wrote before crashing.
	caps, err := capability.NewAssembler(nil).Assemble(catalog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSession(state.Session{
		ConversationID: "chat", AgentID: "codex", HarnessID: "codex",
		UpstreamID: "sess-1", Workspace: workspace, Tainted: true,
		CapabilityHash: caps.Fingerprint,
	}); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{reply: "resumed"}
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": runner}}
	coordinator := newCoordinatorIn(t, map[string]string{"codex": workspace}, catalog, store, capability.NewAssembler(nil), manager, time.Minute)
	// Tainted blocks the turn until the resume path clears it.
	if _, err := handle(coordinator, t.Context(), "继续"); err == nil {
		t.Fatal("tainted session ran without revive")
	}
	if err := coordinator.ReviveSession("chat", "codex"); err != nil {
		t.Fatal(err)
	}
	if store.Conversation("chat").Sessions["codex"].Tainted {
		t.Fatal("taint survived revive")
	}
	// CapabilityHash was saved before the crash never happened here, so the
	// fresh hash matches an empty one only via drift — accept either error
	// class except taint: what matters is the block is no longer the taint.
	if _, err := handle(coordinator, t.Context(), "继续"); err != nil {
		t.Fatalf("revived turn failed: %v", err)
	}
	if got := manager.opened; len(got) == 0 || !strings.Contains(got[len(got)-1], "codex:sess-1") {
		t.Fatalf("revived turn did not reopen the saved session: %v", got)
	}
}

// TestCrashResumeE2E is the acceptance shape scaled down: a task is mid-turn
// when the gateway dies; the next process closes the orphan attempt, revives
// the session and the continuation runs against the same upstream session.
func TestCrashResumeE2E(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "mockagent")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent")
	cmd.Dir = "../.."
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build mockagent: %v\n%s", err, output)
	}
	workspace := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "state.json")
	tasksPath := filepath.Join(t.TempDir(), "tasks.json")
	catalog, _ := agent.NewCatalog(map[string]agent.Config{
		"mock": {Harness: "mock", Default: true},
	})
	configs := map[string]harness.Config{"mock": {Command: bin, Permission: "auto"}}

	// Life before the crash: one finished turn, then a turn "in flight".
	store1, _ := state.Open(statePath)
	tasks1, _ := task.Open(tasksPath)
	manager1, err := harness.NewManager(configs)
	if err != nil {
		t.Fatal(err)
	}
	c1 := newCoordinatorIn(t, map[string]string{"mock": workspace}, catalog, store1, capability.NewAssembler(nil), manager1, 30*time.Second)
	c1.SetTasks(tasks1, "n1")
	result, err := c1.Handle(context.Background(), Request{
		ConversationID: "chat", Input: "hello", MessageID: "om_1", ChatID: "oc_1", ChatType: protocol.ChatGroup,
	})
	if err != nil || !strings.Contains(result.Text, "echo:") {
		t.Fatalf("first turn = %#v, %v", result, err)
	}
	tracked, ok := tasks1.Active("chat", "mock", "")
	if !ok {
		t.Fatal("no task")
	}
	if _, err := tasks1.Begin(tracked.ID, "mock", "n1", ""); err != nil {
		t.Fatal(err)
	}
	session := store1.Conversation("chat").Sessions["mock"]
	session.Tainted = true
	if err := store1.SaveSession(session); err != nil {
		t.Fatal(err)
	}
	manager1.Stop() // the crash

	// The next gateway process.
	store2, _ := state.Open(statePath)
	tasks2, _ := task.Open(tasksPath)
	manager2, err := harness.NewManager(configs)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager2.Stop)
	c2 := restartCoordinator(t, c1, catalog, store2, capability.NewAssembler(nil), manager2, 30*time.Second)
	c2.SetTasks(tasks2, "n1")

	interrupted := tasks2.Interrupted()
	if len(interrupted) != 1 || interrupted[0].ID != tracked.ID || interrupted[0].AnchorMessage != "om_1" {
		t.Fatalf("interrupted = %+v", interrupted)
	}
	if _, err := tasks2.Finish(interrupted[0].ID, task.OutcomeInterrupted, task.Tokens{}, 0); err != nil {
		t.Fatal(err)
	}
	if err := c2.ReviveSession("chat", "mock"); err != nil {
		t.Fatal(err)
	}
	result, err = c2.Handle(context.Background(), Request{
		ConversationID: "chat", Input: "继续任务", MessageID: "om_2", ChatID: "oc_1", ChatType: protocol.ChatGroup,
	})
	if err != nil || !strings.Contains(result.Text, "echo:") {
		t.Fatalf("resumed turn = %#v, %v", result, err)
	}
	// Same upstream session — the agent kept its context across the crash.
	if got := store2.Conversation("chat").Sessions["mock"].UpstreamID; got != session.UpstreamID {
		t.Fatalf("upstream drifted: %q -> %q", session.UpstreamID, got)
	}
	// The history is honest: an interrupted attempt, then a finished one.
	final, _ := tasks2.Get(tracked.ID)
	outcomes := []task.Outcome{}
	for _, attempt := range final.Attempts {
		outcomes = append(outcomes, attempt.Outcome)
	}
	if len(outcomes) != 3 || outcomes[1] != task.OutcomeInterrupted || outcomes[2] != task.OutcomeOK {
		t.Fatalf("attempt history = %v", outcomes)
	}
}

func TestOnboardingTurnOpensNoTask(t *testing.T) {
	runner := &fakeRunner{reply: "ok"}
	coordinator, tasks := taskCoordinator(t, runner)
	if _, err := coordinator.Handle(t.Context(), Request{
		ConversationID: "steve:onboard:ou_x", Input: "自我介绍", SenderOpenID: "ou_x",
		ChatType: protocol.ChatP2P,
	}); err != nil {
		t.Fatal(err)
	}
	if all := tasks.List(""); len(all) != 0 {
		t.Fatalf("onboarding opened a task: %+v", all)
	}
}

func TestIncompleteProfileOnlyInterceptsHomeProject(t *testing.T) {
	for _, projectID := range []string{"codex", "home"} {
		t.Run(projectID, func(t *testing.T) {
			runner := &fakeRunner{reply: "ok"}
			coordinator, tasks := taskCoordinator(t, runner)
			dir := t.TempDir()
			if err := home.BootstrapLocale(dir, "owner", home.LocaleZH); err != nil {
				t.Fatal(err)
			}
			useHome(t, coordinator, dir)
			coordinator.SetIdentity("owner", home.Dir{Path: dir})
			if _, err := coordinator.projects.Bind(t.Context(), "chat", projectID, "owner"); err != nil {
				t.Fatal(err)
			}
			result, err := coordinator.Handle(t.Context(), Request{
				ConversationID: "chat", Input: "don't scan; explain the project", SenderOpenID: "owner", ChatType: protocol.ChatP2P,
			})
			if err != nil {
				t.Fatal(err)
			}
			building := strings.Contains(result.Injected.Prompt, "Build their profile now.")
			if building != (projectID == "home") {
				t.Fatalf("project=%s profile intercepted=%v", projectID, building)
			}
			all := tasks.List("chat")
			if len(all) != 1 || all[0].ProjectID != projectID {
				t.Fatalf("task attribution: %+v", all)
			}
		})
	}
}
