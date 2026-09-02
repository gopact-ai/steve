package turn

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/state"
)

// TestProjectVerbShowsBindsAndArchives: /project reports the binding, and
// /project use moves it — version up, live sessions archived so the next
// turn opens in the new directory, and the task that was open keeps the
// project it was created under.
func TestProjectVerbShowsBindsAndArchives(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{
		"codex": {Harness: "codex", Default: true},
		"other": {Harness: "codex", Aliases: []string{"other"}},
	})
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": {reply: "ok"}}}
	coordinator := newCoordinator(t, catalog, store, capability.NewAssembler(nil), manager, time.Minute)

	// Before any turn: unbound, but the list is there.
	result, err := handle(coordinator, t.Context(), "/project")
	if err != nil || !strings.Contains(result.Text, "/project use") || !strings.Contains(result.Text, "  other —") {
		t.Fatalf("unbound status = %#v, %v", result, err)
	}
	// A turn binds the default and opens in its directory.
	if _, err := handle(coordinator, t.Context(), "hello"); err != nil {
		t.Fatal(err)
	}
	if got := manager.workdirs[0]; got != workspaceOf(t, coordinator, "codex") {
		t.Fatalf("first turn dir = %q, want the default project's home", got)
	}
	result, _ = handle(coordinator, t.Context(), "/project")
	if !strings.Contains(result.Text, "* codex —") || !strings.Contains(result.Text, "第 1 版") {
		t.Fatalf("bound status = %q", result.Text)
	}
	if saved := store.Conversation("chat").Sessions["codex"]; saved.ProjectID != "codex" || saved.ProjectVersion != 1 {
		t.Fatalf("session binding = %+v", saved)
	}

	// Unknown project: a message, nothing changes.
	result, _ = handle(coordinator, t.Context(), "/project use nope")
	if !strings.Contains(result.Text, "nope") {
		t.Fatalf("unknown project = %q", result.Text)
	}
	if _, ok := store.Conversation("chat").Sessions["codex"]; !ok {
		t.Fatal("a refused switch archived the session")
	}

	// Switch: version 2, session archived, next turn in the new directory.
	result, err = handle(coordinator, t.Context(), "/project use other")
	if err != nil || !strings.Contains(result.Text, "other") {
		t.Fatalf("switch = %#v, %v", result, err)
	}
	conversation := store.Conversation("chat")
	if _, ok := conversation.Sessions["codex"]; ok || len(conversation.Archived) != 1 {
		t.Fatalf("switch did not archive the live session: %+v", conversation)
	}
	if _, err := handle(coordinator, t.Context(), "again"); err != nil {
		t.Fatal(err)
	}
	if got := manager.workdirs[len(manager.workdirs)-1]; got != workspaceOf(t, coordinator, "other") {
		t.Fatalf("turn after switch dir = %q, want project other's home", got)
	}
	if saved := store.Conversation("chat").Sessions["codex"]; saved.ProjectID != "other" || saved.ProjectVersion != 2 {
		t.Fatalf("session binding after switch = %+v", saved)
	}
}

// TestProjectSwitchRefusedWhileATurnRuns: a session in flight cannot be
// pulled out from under its directory.
func TestProjectSwitchRefusedWhileATurnRuns(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{
		"codex": {Harness: "codex", Default: true},
		"other": {Harness: "codex", Aliases: []string{"other"}},
	})
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	slow := &fakeRunner{reply: "done", started: make(chan struct{}), done: make(chan struct{})}
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": slow}}
	coordinator := newCoordinator(t, catalog, store, capability.NewAssembler(nil), manager, time.Minute)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_, _ = handle(coordinator, t.Context(), "slow work")
	}()
	<-slow.started
	result, err := handle(coordinator, t.Context(), "/project use other")
	if err != nil || !strings.Contains(result.Text, "/cancel") {
		t.Fatalf("switch during a turn = %#v, %v; want the busy refusal", result, err)
	}
	close(slow.done)
	<-finished
	if b, ok, _ := coordinator.projects.Binding(t.Context(), "chat"); !ok || b.ProjectID != "codex" {
		t.Fatalf("binding moved during a turn: %+v", b)
	}
}

// TestChatTurnIsAnAttemptUnderTheCanonicalLock: a turn is recorded as an
// attempt fenced on the project's canonical lock; while it runs, another
// conversation on the same project is told who holds it; when it ends the
// attempt is bound and the lock is free.
func TestChatTurnIsAnAttemptUnderTheCanonicalLock(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{
		"codex": {Harness: "codex", Default: true},
	})
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	slow := &fakeRunner{reply: "done", started: make(chan struct{}), done: make(chan struct{})}
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": slow}}
	coordinator := newCoordinator(t, catalog, store, capability.NewAssembler(nil), manager, time.Minute)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_, _ = handle(coordinator, t.Context(), "slow work")
	}()
	<-slow.started

	// Another conversation, same default project: refused, with the holder named.
	_, err := coordinator.Handle(t.Context(), Request{ConversationID: "other-chat", Input: "me too", MessageID: "om_2"})
	if err == nil || !strings.Contains(err.Error(), "codex") || !strings.Contains(err.Error(), "/project") {
		t.Fatalf("second conversation = %v; want the busy refusal naming the holder", err)
	}
	live, _ := coordinator.attempts.Live(t.Context())
	if len(live) != 1 || live[0].Kind != "chat" || live[0].Scope != "unrestricted" || live[0].State != "running" {
		t.Fatalf("live attempts = %+v", live)
	}
	close(slow.done)
	<-finished

	// Bound, with the canonical lock among its fencings; lock released.
	done, _ := coordinator.attempts.Live(t.Context())
	if len(done) != 0 {
		t.Fatalf("attempts still live after the turn: %+v", done)
	}
	events, _ := coordinator.attempts.History(t.Context(), live[0].ID)
	last := events[len(events)-1]
	if last.To != "bound" || len(last.Fencings) != 2 || last.Fencings[1].Key != "canonical:codex" {
		t.Fatalf("final event = %+v", last)
	}
	if _, err := coordinator.Handle(t.Context(), Request{ConversationID: "other-chat", Input: "now?", MessageID: "om_3"}); err != nil {
		t.Fatalf("turn after the lock was released: %v", err)
	}
	// A failing turn records a failed attempt.
	manager.runners["codex"] = &fakeRunner{err: errBoom}
	_, _ = coordinator.Handle(t.Context(), Request{ConversationID: "third", Input: "break", MessageID: "om_4"})
	// No task store in this fixture, so every attempt sits under the empty
	// task id; one of them must be the failure.
	all, _ := coordinator.attempts.ForTask(t.Context(), "")
	failed := 0
	for _, a := range all {
		if a.State == "failed" {
			failed++
		}
	}
	if failed != 1 {
		t.Fatalf("failed attempts = %d, want 1", failed)
	}
}

var errBoom = fmt.Errorf("boom")
