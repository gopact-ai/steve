package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gopact-ai/acp"
)

func TestNativeMemoryFixtureLoadsOnlyExactSession(t *testing.T) {
	t.Setenv("MOCKAGENT_MEMORY_DIR", t.TempDir())
	first := &agent{}
	created, err := first.NewSession(t.Context(), &acp.NewSessionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	memory, err := readMemory(created.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	memory.Marker, memory.Model, memory.Mode = "isolated-marker", "mock-deep", "read-only"
	if err := writeMemory(memory); err != nil {
		t.Fatal(err)
	}
	// A fresh handler has no process-local state. Load must read only the
	// exact session's durable record, never the latest record in a directory.
	restarted := &agent{}
	loaded, err := restarted.LoadSession(t.Context(), &acp.LoadSessionRequest{SessionID: created.SessionID})
	if err != nil || loaded.ConfigOptions == nil ||
		chosen(&restarted.model, "") != "mock-deep" || chosen(&restarted.mode, "") != "read-only" {
		t.Fatalf("load did not restore selectors: %v", err)
	}
	restored, err := readMemory(created.SessionID)
	if err != nil || restored.Marker != memory.Marker {
		t.Fatalf("native memory missing after load: %v", err)
	}
	fresh, err := restarted.NewSession(t.Context(), &acp.NewSessionRequest{})
	if err != nil || fresh.SessionID == created.SessionID {
		t.Fatalf("new session reused a prior process's identity: %v", err)
	}
	empty, err := readMemory(fresh.SessionID)
	if err != nil || empty.Marker != "" {
		t.Fatalf("new session inherited another session's marker: %v", err)
	}
	if _, err := restarted.LoadSession(t.Context(), &acp.LoadSessionRequest{SessionID: "unknown"}); err == nil {
		t.Fatal("unknown native session accepted")
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("MOCKAGENT_MEMORY_DIR"), "reject-load"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.LoadSession(t.Context(), &acp.LoadSessionRequest{SessionID: created.SessionID}); err == nil {
		t.Fatal("injected native load failure ignored")
	}
}

func TestNativeMemoryFixtureIsOptIn(t *testing.T) {
	t.Setenv("MOCKAGENT_MEMORY_DIR", "")
	a := &agent{}
	created, err := a.NewSession(t.Context(), &acp.NewSessionRequest{})
	if err != nil || created.SessionID != "mock-session-1" {
		t.Fatalf("default fixture behavior changed: %v", err)
	}
	if _, err := a.LoadSession(t.Context(), &acp.LoadSessionRequest{SessionID: "ordinary-native-session"}); err != nil {
		t.Fatalf("default load behavior changed: %v", err)
	}
}
