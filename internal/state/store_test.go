package state

import (
	"path/filepath"
	"testing"
)

func TestStorePersistsConversationSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetActiveAgent("chat-1", "claude"); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSession(Session{
		ConversationID: "chat-1",
		AgentID:        "claude",
		HarnessID:      "claude-code",
		UpstreamID:     "session-1",
		Workspace:      "/work",
	}); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	conversation := reopened.Conversation("chat-1")
	if conversation.ActiveAgent != "claude" {
		t.Fatalf("unexpected active agent: %q", conversation.ActiveAgent)
	}
	session, ok := conversation.Sessions["claude"]
	if !ok || session.UpstreamID != "session-1" || session.HarnessID != "claude-code" {
		t.Fatalf("unexpected session: %#v, %v", session, ok)
	}
}

func TestStoreRejectsHarnessChange(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	session := Session{ConversationID: "chat-1", AgentID: "claude", HarnessID: "claude-code"}
	if err := store.SaveSession(session); err != nil {
		t.Fatal(err)
	}
	session.HarnessID = "codex"
	if err := store.SaveSession(session); err == nil {
		t.Fatal("expected immutable harness error")
	}
}

func TestStorePairingRequestAndApprove(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	code, err := store.RequestPairing("ou_user")
	if err != nil || len(code) != 8 {
		t.Fatalf("request = %q, %v", code, err)
	}
	again, err := store.RequestPairing("ou_user")
	if err != nil || again != code {
		t.Fatalf("reuse = %q, %v", again, err)
	}
	openID, err := store.ApprovePairing(code)
	if err != nil || openID != "ou_user" {
		t.Fatalf("approve = %q, %v", openID, err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.Allows("ou_user") {
		t.Fatal("approved sender was not persisted")
	}
	if _, err := reopened.ApprovePairing("NOPECODE"); err == nil {
		t.Fatal("expected unknown pairing code")
	}
}

func TestStorePairingApproveIsVisibleToOtherHandle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	gateway, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	code, err := gateway.RequestPairing("ou_user")
	if err != nil {
		t.Fatal(err)
	}
	cli, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cli.ApprovePairing(code); err != nil {
		t.Fatal(err)
	}
	if !gateway.Allows("ou_user") {
		t.Fatal("running process did not observe pairing approval")
	}
}

func TestStoreRejectsWorkspaceChange(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	session := Session{ConversationID: "chat-1", AgentID: "claude", HarnessID: "claude-code", Workspace: "/one"}
	if err := store.SaveSession(session); err != nil {
		t.Fatal(err)
	}
	session.Workspace = "/two"
	if err := store.SaveSession(session); err == nil {
		t.Fatal("expected immutable workspace error")
	}
}

func TestStoreOnboardedPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if store.Onboarded() {
		t.Fatal("fresh store should not be onboarded")
	}
	if err := store.MarkOnboarded(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.Onboarded() {
		t.Fatal("onboarded flag was not persisted")
	}
}

func TestStoreRelocateMovesConversation(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetActiveAgent("from", "codex"); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSession(Session{
		ConversationID: "from", AgentID: "codex", HarnessID: "codex",
		UpstreamID: "up", Workspace: "/home",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Relocate("from", "to"); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Conversation("from").Sessions["codex"]; ok {
		t.Fatal("source conversation still present")
	}
	got := store.Conversation("to")
	if got.ActiveAgent != "codex" || got.Sessions["codex"].ConversationID != "to" || got.Sessions["codex"].UpstreamID != "up" {
		t.Fatalf("relocated = %#v", got)
	}
}

func TestStoreRelocateOverwritesDestination(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSession(Session{
		ConversationID: "from", AgentID: "codex", HarnessID: "codex", UpstreamID: "home", Workspace: "/home",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSession(Session{
		ConversationID: "to", AgentID: "codex", HarnessID: "codex", UpstreamID: "old", Workspace: "/work",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Relocate("from", "to"); err != nil {
		t.Fatal(err)
	}
	got := store.Conversation("to").Sessions["codex"]
	if got.UpstreamID != "home" || got.Workspace != "/home" || got.ConversationID != "to" {
		t.Fatalf("destination = %#v", got)
	}
	if _, ok := store.Conversation("from").Sessions["codex"]; ok {
		t.Fatal("source conversation still present")
	}
}

func TestPreferencesPersistPerAgent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetPreferences("chat", "codex", map[string]string{"model": "gpt-6", "reasoning": "high"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetPreferences("chat", "codex", map[string]string{"reasoning": ""}); err != nil {
		t.Fatal(err)
	}
	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got := again.Preferences("chat", "codex")
	if got["model"] != "gpt-6" || got["reasoning"] != "" || len(got) != 1 {
		t.Fatalf("preferences after reopen = %v", got)
	}
	if other := again.Preferences("chat", "claude"); len(other) != 0 {
		t.Fatalf("another agent's preferences = %v", other)
	}
}
