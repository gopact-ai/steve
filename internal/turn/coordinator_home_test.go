package turn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/state"
)

func TestInjectionMode(t *testing.T) {
	tests := []struct {
		chat, sender, owner string
		want                home.Mode
	}{
		{"p2p", "ou_me", "ou_me", home.ModeOwner},
		{"group", "ou_me", "ou_me", home.ModeGuest},
		{"", "ou_me", "ou_me", home.ModeGuest},
		{"topic", "ou_me", "ou_me", home.ModeGuest},
		{"p2p", "ou_me", "", home.ModeGuest},
		{"p2p", "ou_other", "ou_me", home.ModeGuest},
	}
	for _, tt := range tests {
		if got := injectionMode(tt.chat, tt.sender, tt.owner); got != tt.want {
			t.Fatalf("injectionMode(%q,%q,%q)=%q want %q", tt.chat, tt.sender, tt.owner, got, tt.want)
		}
	}
}

func TestOwnerFirstTurnInjectsMemoryThenStops(t *testing.T) {
	dir := t.TempDir()
	if err := home.Bootstrap(dir, "ou_me"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, home.FileMemory), []byte("remember-this"), 0o600); err != nil {
		t.Fatal(err)
	}
	coordinator, store, runner := homeCoordinator(t, dir, "ou_me")
	first, err := coordinator.Handle(t.Context(), Request{
		ConversationID: "dm", Input: "hello", SenderOpenID: "ou_me", ChatType: "p2p",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.prompts) == 0 || !strings.Contains(runner.prompts[0], "remember-this") || !strings.Contains(runner.prompts[0], "Steve home") {
		t.Fatalf("first prompt missing home: %v", runner.prompts)
	}
	if !strings.Contains(runner.prompts[0], "[steve: speaker=ou_me owner=true chat=p2p]") {
		t.Fatalf("missing speaker line: %s", runner.prompts[0])
	}
	if first.Text == "" {
		t.Fatal("empty reply")
	}
	upstream := store.Conversation("dm").Sessions["codex"].UpstreamID
	if _, err := coordinator.Handle(t.Context(), Request{
		ConversationID: "dm", Input: "again", SenderOpenID: "ou_me", ChatType: "p2p",
	}); err != nil {
		t.Fatal(err)
	}
	if len(runner.prompts) < 2 || strings.Contains(runner.prompts[1], "remember-this") {
		t.Fatal("second turn re-prepended home")
	}
	if store.Conversation("dm").Sessions["codex"].UpstreamID != upstream {
		t.Fatal("second turn opened a new session")
	}
}

func TestNewReloadsMemory(t *testing.T) {
	dir := t.TempDir()
	if err := home.Bootstrap(dir, "ou_me"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, home.FileMemory), []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	coordinator, store, runner := homeCoordinator(t, dir, "ou_me")
	if _, err := coordinator.Handle(t.Context(), Request{
		ConversationID: "dm", Input: "hello", SenderOpenID: "ou_me", ChatType: "p2p",
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Conversation("dm").Sessions["codex"]; !ok {
		t.Fatal("missing session before /new")
	}
	reset, err := coordinator.Handle(t.Context(), Request{
		ConversationID: "dm", Input: "/new", SenderOpenID: "ou_me", ChatType: "p2p",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reset.Text, "身份与记忆保留") || strings.Contains(reset.Text, dir) {
		t.Fatalf("reset text = %q", reset.Text)
	}
	if err := os.WriteFile(filepath.Join(dir, home.FileMemory), []byte("after"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Handle(t.Context(), Request{
		ConversationID: "dm", Input: "next", SenderOpenID: "ou_me", ChatType: "p2p",
	}); err != nil {
		t.Fatal(err)
	}
	if len(runner.prompts) < 2 || !strings.Contains(runner.prompts[len(runner.prompts)-1], "after") {
		t.Fatalf("did not reload MEMORY: %v", runner.prompts)
	}
	if store.Conversation("dm").Sessions["codex"].InstructionsApplied != true {
		t.Fatal("expected instructions applied after reload")
	}
}

func TestGuestOmitsMemoryAndPath(t *testing.T) {
	dir := t.TempDir()
	if err := home.Bootstrap(dir, "ou_me"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, home.FileMemory), []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	coordinator, _, runner := homeCoordinator(t, dir, "ou_me")
	if _, err := coordinator.Handle(t.Context(), Request{
		ConversationID: "grp", Input: "hello", SenderOpenID: "ou_me", ChatType: "group",
	}); err != nil {
		t.Fatal(err)
	}
	if len(runner.prompts) == 0 || strings.Contains(runner.prompts[0], "private") || strings.Contains(runner.prompts[0], dir) {
		t.Fatalf("group leaked home: %v", runner.prompts)
	}
	status, err := coordinator.Handle(t.Context(), Request{
		ConversationID: "grp", Input: "/status", SenderOpenID: "ou_me", ChatType: "group",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status.Text, "home=guest") || strings.Contains(status.Text, dir) {
		t.Fatalf("group status = %q", status.Text)
	}
}

func TestGroupSendersShareFingerprint(t *testing.T) {
	dir := t.TempDir()
	if err := home.Bootstrap(dir, "ou_me"); err != nil {
		t.Fatal(err)
	}
	coordinator, store, runner := homeCoordinator(t, dir, "ou_me")
	if _, err := coordinator.Handle(t.Context(), Request{
		ConversationID: "grp", Input: "one", SenderOpenID: "ou_a", ChatType: "group",
	}); err != nil {
		t.Fatal(err)
	}
	hash := store.Conversation("grp").Sessions["codex"].CapabilityHash
	if _, err := coordinator.Handle(t.Context(), Request{
		ConversationID: "grp", Input: "two", SenderOpenID: "ou_b", ChatType: "group",
	}); err != nil {
		t.Fatal(err)
	}
	if store.Conversation("grp").Sessions["codex"].CapabilityHash != hash {
		t.Fatal("speaker line changed capability hash")
	}
	if len(runner.prompts) < 2 || !strings.Contains(runner.prompts[1], "speaker=ou_b") {
		t.Fatalf("second speaker missing: %v", runner.prompts)
	}
}

func homeCoordinator(t *testing.T, homeDir, owner string) (*Coordinator, *state.Store, *fakeRunner) {
	t.Helper()
	catalog, err := agent.NewCatalog(map[string]agent.Config{
		"codex": {Harness: "codex", Workspace: t.TempDir(), Default: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{id: "sess"}
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": runner}}
	assembler := capability.NewAssembler(nil).SetHome(home.Dir{Path: homeDir})
	coordinator := New(catalog, store, assembler, manager, time.Minute)
	coordinator.SetIdentity(owner, home.Dir{Path: homeDir})
	return coordinator, store, runner
}
