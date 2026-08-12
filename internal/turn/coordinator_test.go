package turn

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/state"
)

type fakeManager struct {
	runners map[string]*fakeRunner
	opened  []string
}

func (m *fakeManager) OpenSession(_ context.Context, harnessID, upstreamID, _ string, _ []acp.MCPServer) (harness.Runner, error) {
	id := upstreamID
	if id == "" {
		id = harnessID + "-session"
	}
	runner := m.runners[harnessID]
	runner.id = id
	m.opened = append(m.opened, harnessID+":"+upstreamID)
	return runner, nil
}

func (m *fakeManager) CloseSession(context.Context, string, string) error { return nil }

type fakeRunner struct {
	id      string
	prompts []string
	started chan struct{}
	done    chan struct{}
	err     error
	cancels atomic.Int32
	aborts  atomic.Int32
	stop    sync.Once
}

func (r *fakeRunner) ID() string { return r.id }
func (r *fakeRunner) Prompt(_ context.Context, prompt string) (string, []string, error) {
	r.prompts = append(r.prompts, prompt)
	if r.started != nil {
		close(r.started)
		<-r.done
	}
	return "reply: " + prompt, nil, r.err
}
func (r *fakeRunner) Cancel(context.Context) error {
	r.cancels.Add(1)
	if r.done != nil {
		r.stop.Do(func() { close(r.done) })
	}
	return nil
}
func (r *fakeRunner) Abort() { r.aborts.Add(1) }

func TestCoordinatorCancelsFailedTurnAndDiscardsSession(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{
		"codex": {Harness: "codex", Workspace: t.TempDir(), SystemPrompt: "rules", Default: true},
	})
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	runner := &fakeRunner{err: errors.New("prompt failed")}
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": runner}}
	coordinator := New(catalog, store, capability.NewAssembler(nil), manager, time.Minute)
	if _, err := coordinator.Handle(t.Context(), "chat", "hello"); err == nil {
		t.Fatal("expected prompt error")
	}
	if runner.cancels.Load() < 1 {
		t.Fatal("cancel was not called")
	}
	if runner.aborts.Load() < 1 {
		t.Fatal("abort was not called")
	}
	if _, ok := store.Conversation("chat").Sessions["codex"]; ok {
		t.Fatal("failed turn session was retained")
	}
}

func TestCoordinatorRejectsTaintedSession(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{
		"codex": {Harness: "codex", Workspace: t.TempDir(), Default: true},
	})
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err := store.SaveSession(state.Session{
		ConversationID: "chat", AgentID: "codex", HarnessID: "codex", Workspace: catalog.Default().Workspace,
		CapabilityHash: "ignored", Tainted: true,
	}); err != nil {
		t.Fatal(err)
	}
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": {}}}
	coordinator := New(catalog, store, capability.NewAssembler(nil), manager, time.Minute)
	if _, err := coordinator.Handle(t.Context(), "chat", "hello"); err == nil || !strings.Contains(err.Error(), "uncertain") {
		t.Fatalf("expected tainted session error, got %v", err)
	}
}

func TestCoordinatorSwitchesAgentsAndRestoresSessions(t *testing.T) {
	catalog, err := agent.NewCatalog(map[string]agent.Config{
		"codex":  {Harness: "codex", Default: true},
		"claude": {Harness: "claude", SystemPrompt: "Act as Claude."},
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": {}, "claude": {}}}
	coordinator := New(catalog, store, capability.NewAssembler(nil), manager, time.Minute)

	first, err := coordinator.Handle(t.Context(), "chat", "hello")
	if err != nil || first.AgentID != "codex" {
		t.Fatalf("default turn = %#v, %v", first, err)
	}
	second, err := coordinator.Handle(t.Context(), "chat", "@claude fix it")
	if err != nil || second.AgentID != "claude" {
		t.Fatalf("tag turn = %#v, %v", second, err)
	}
	if got := manager.runners["claude"].prompts[0]; !strings.Contains(got, "Act as Claude.") || !strings.Contains(got, "fix it") {
		t.Fatalf("unexpected Claude prompt: %q", got)
	}
	if _, err := coordinator.Handle(t.Context(), "chat", "@codex again"); err != nil {
		t.Fatal(err)
	}
	if got := manager.opened[len(manager.opened)-1]; got != "codex:codex-session" {
		t.Fatalf("Codex session was not restored: %q", got)
	}
	if _, err := coordinator.Handle(t.Context(), "chat", "second"); err != nil {
		t.Fatal(err)
	}
	if got := manager.runners["codex"].prompts[2]; strings.Contains(got, "Act as") {
		t.Fatalf("instructions were injected twice: %q", got)
	}
}

func TestCoordinatorRejectsCapabilityDrift(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Default: true}})
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": {}}}
	coordinator := New(catalog, store, capability.NewAssembler(nil), manager, time.Minute)
	if _, err := coordinator.Handle(t.Context(), "chat", "hello"); err != nil {
		t.Fatal(err)
	}
	conversation := store.Conversation("chat")
	session := conversation.Sessions["codex"]
	session.CapabilityHash = "stale"
	if err := store.DeleteSession("chat", "codex"); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSession(session); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Handle(t.Context(), "chat", "again"); err == nil {
		t.Fatal("expected capability drift error")
	}
}

func TestCoordinatorCancelsRunningTurn(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Default: true}})
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	runner := &fakeRunner{started: make(chan struct{}), done: make(chan struct{})}
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": runner}}
	coordinator := New(catalog, store, capability.NewAssembler(nil), manager, time.Minute)
	turnDone := make(chan error, 1)
	go func() {
		_, err := coordinator.Handle(t.Context(), "chat", "long task")
		turnDone <- err
	}()
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("turn did not start")
	}
	result, err := coordinator.Handle(t.Context(), "chat", "/cancel")
	if err != nil || !strings.Contains(result.Text, "已请求取消") {
		t.Fatalf("cancel = %#v, %v", result, err)
	}
	select {
	case err := <-turnDone:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("turn did not finish after cancel")
	}
	if _, ok := store.Conversation("chat").Sessions["codex"]; ok {
		t.Fatal("prompt goroutine did not clean up the canceled session")
	}
}

func TestCoordinatorCancelWithoutRunningTurn(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Default: true}})
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": {}}}
	coordinator := New(catalog, store, capability.NewAssembler(nil), manager, time.Minute)
	result, err := coordinator.Handle(t.Context(), "chat", "/cancel")
	if err != nil || !strings.Contains(result.Text, "没有运行中的任务") {
		t.Fatalf("cancel = %#v, %v", result, err)
	}
}

func TestCoordinatorRejectsConcurrentTurnForSession(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Default: true}})
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	runner := &fakeRunner{started: make(chan struct{}), done: make(chan struct{})}
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": runner}}
	coordinator := New(catalog, store, capability.NewAssembler(nil), manager, time.Minute)
	turnDone := make(chan error, 1)
	go func() {
		_, err := coordinator.Handle(t.Context(), "chat", "first")
		turnDone <- err
	}()
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("first turn did not start")
	}
	if _, err := coordinator.Handle(t.Context(), "chat", "second"); err == nil {
		t.Fatal("expected concurrent turn error")
	}
	close(runner.done)
	if err := <-turnDone; err != nil {
		t.Fatal(err)
	}
}

func TestCoordinatorSwitchOnlyAndNew(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{
		"codex": {Harness: "codex", Default: true}, "claude": {Harness: "claude"},
	})
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": {}, "claude": {}}}
	coordinator := New(catalog, store, capability.NewAssembler(nil), manager, time.Minute)

	result, err := coordinator.Handle(t.Context(), "chat", "/use claude")
	if err != nil || result.Text != "已切换到 claude" {
		t.Fatalf("switch = %#v, %v", result, err)
	}
	if _, err := coordinator.Handle(t.Context(), "chat", "hello"); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Handle(t.Context(), "chat", "/new"); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Conversation("chat").Sessions["claude"]; ok {
		t.Fatal("/new did not delete active agent session")
	}
}
