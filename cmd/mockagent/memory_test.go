package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

// A session's events say where it was opened or loaded, so a reader can tell
// sessions apart by working directory; prompts carry no directory.
func TestNativeMemoryEventsRecordWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MOCKAGENT_MEMORY_DIR", dir)
	a := &agent{}
	created, err := a.NewSession(t.Context(), &acp.NewSessionRequest{Cwd: "/opened/here"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.memoryPrompt(t.Context(), &acp.PromptRequest{SessionID: created.SessionID}, "hello"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.LoadSession(t.Context(), &acp.LoadSessionRequest{SessionID: created.SessionID, Cwd: "/loaded/here"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	type event struct {
		Kind string `json:"kind"`
		Cwd  string `json:"cwd"`
	}
	var got []event
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var e event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		got = append(got, e)
	}
	want := []event{{"new", "/opened/here"}, {"prompt", ""}, {"load", "/loaded/here"}}
	if len(got) != len(want) {
		t.Fatalf("events = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events = %+v, want %+v", got, want)
		}
	}
}
