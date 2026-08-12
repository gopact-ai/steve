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
