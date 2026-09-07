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
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/state"
)

func TestInjectionMode(t *testing.T) {
	tests := []struct {
		name          string
		chat          protocol.ChatType
		sender, owner string
		want          home.Mode
	}{
		{name: "owner p2p", chat: protocol.ChatP2P, sender: "ou_me", owner: "ou_me", want: home.ModeOwner},
		{name: "owner group", chat: protocol.ChatGroup, sender: "ou_me", owner: "ou_me", want: home.ModeGuest},
		{name: "empty chat", chat: protocol.ChatUnknown, sender: "ou_me", owner: "ou_me", want: home.ModeGuest},
		{name: "unknown chat", chat: protocol.ChatType("topic"), sender: "ou_me", owner: "ou_me", want: home.ModeGuest},
		{name: "unset owner", chat: protocol.ChatP2P, sender: "ou_me", owner: "", want: home.ModeGuest},
		{name: "other sender", chat: protocol.ChatP2P, sender: "ou_other", owner: "ou_me", want: home.ModeGuest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := injectionMode(tt.chat, tt.sender, tt.owner); got != tt.want {
				t.Fatalf("injectionMode(%q,%q,%q)=%q want %q", tt.chat, tt.sender, tt.owner, got, tt.want)
			}
		})
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
		ConversationID: "dm", Input: "hello", SenderOpenID: "ou_me", ChatType: protocol.ChatP2P,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.seen()) == 0 || !strings.Contains(runner.seen()[0], "remember-this") || !strings.Contains(runner.seen()[0], "Steve home") {
		t.Fatalf("first prompt missing home: %v", runner.seen())
	}
	if !strings.Contains(runner.seen()[0], "[steve: speaker=ou_me owner=true chat=p2p]") {
		t.Fatalf("missing speaker line: %s", runner.seen()[0])
	}
	if first.Text == "" {
		t.Fatal("empty reply")
	}
	upstream := store.Conversation("dm").Sessions["codex"].UpstreamID
	if _, err := coordinator.Handle(t.Context(), Request{
		ConversationID: "dm", Input: "again", SenderOpenID: "ou_me", ChatType: protocol.ChatP2P,
	}); err != nil {
		t.Fatal(err)
	}
	if len(runner.seen()) < 2 || strings.Contains(runner.seen()[1], "remember-this") {
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
		ConversationID: "dm", Input: "hello", SenderOpenID: "ou_me", ChatType: protocol.ChatP2P,
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Conversation("dm").Sessions["codex"]; !ok {
		t.Fatal("missing session before /new")
	}
	reset, err := coordinator.Handle(t.Context(), Request{
		ConversationID: "dm", Input: "/new", SenderOpenID: "ou_me", ChatType: protocol.ChatP2P,
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
		ConversationID: "dm", Input: "next", SenderOpenID: "ou_me", ChatType: protocol.ChatP2P,
	}); err != nil {
		t.Fatal(err)
	}
	if len(runner.seen()) < 2 || !strings.Contains(runner.seen()[len(runner.seen())-1], "after") {
		t.Fatalf("did not reload MEMORY: %v", runner.seen())
	}
	if store.Conversation("dm").Sessions["codex"].InstructionsApplied != true {
		t.Fatal("expected instructions applied after reload")
	}
}

func TestOwnerP2POpensHomeWorkspace(t *testing.T) {
	dir := t.TempDir()
	if err := home.Bootstrap(dir, "ou_me"); err != nil {
		t.Fatal(err)
	}
	coordinator, store, manager := homeCoordinatorWithManager(t, dir, "ou_me")
	if _, err := coordinator.Handle(t.Context(), Request{
		ConversationID: "dm", Input: "hello", SenderOpenID: "ou_me", ChatType: protocol.ChatP2P,
	}); err != nil {
		t.Fatal(err)
	}
	if len(manager.workdirs) == 0 || manager.workdirs[0] != dir {
		t.Fatalf("owner workspace = %v, want home %s", manager.workdirs, dir)
	}
	if store.Conversation("dm").Sessions["codex"].Workspace != dir {
		t.Fatalf("saved workspace = %q", store.Conversation("dm").Sessions["codex"].Workspace)
	}
}

// TestExistingOwnerSessionInAnotherDirectoryIsStale: a session is bound to
// the conversation's project binding. One that was opened somewhere else is
// not silently continued there; the person is asked for /new.
func TestExistingOwnerSessionInAnotherDirectoryIsStale(t *testing.T) {
	dir := t.TempDir()
	if err := home.Bootstrap(dir, "ou_me"); err != nil {
		t.Fatal(err)
	}
	coordinator, store, manager := homeCoordinatorWithManager(t, dir, "ou_me")
	coding := t.TempDir()
	caps, err := capability.NewAssembler(nil).SetHome(home.Dir{Path: dir}).AssembleMode(
		agent.Agent{Harness: "codex"}, home.ModeOwner,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSession(state.Session{
		ConversationID: "dm", AgentID: "codex", HarnessID: "codex",
		UpstreamID: "sess", Workspace: coding, CapabilityHash: caps.Fingerprint,
		InstructionsApplied: true,
	}); err != nil {
		t.Fatal(err)
	}
	_, err = coordinator.Handle(t.Context(), Request{
		ConversationID: "dm", Input: "hello", SenderOpenID: "ou_me", ChatType: protocol.ChatP2P,
	})
	if err == nil || !strings.Contains(err.Error(), "/new") {
		t.Fatalf("expected a stale-workspace refusal pointing at /new, got %v", err)
	}
	if len(manager.workdirs) != 0 {
		t.Fatalf("a stale session was opened in %v", manager.workdirs)
	}
}

func TestOwnerFollowUpWritesPortrait(t *testing.T) {
	dir := t.TempDir()
	if err := home.Bootstrap(dir, "ou_me"); err != nil {
		t.Fatal(err)
	}
	coordinator, _, runner := homeCoordinator(t, dir, "ou_me")
	runner.reply = "已记下，李总。\n\n===SOUL.md===\n# Soul\n你是李总的助手\n===USER.md===\n# User\n- 称呼：李总\n- 时区：Asia/Shanghai\n"
	runner.activity = []string{`sed -n '1,120p' USER.md`}
	result, err := coordinator.Handle(t.Context(), Request{
		ConversationID: "dm", Input: "叫我李总，时区对的", SenderOpenID: "ou_me", ChatType: protocol.ChatP2P,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "已记下，李总。" || len(result.Activity) != 0 {
		t.Fatalf("result = %#v", result)
	}
	if len(runner.seen()) == 0 || !strings.Contains(runner.seen()[0], "===SOUL.md===") {
		t.Fatalf("missing draft instruction: %v", runner.seen())
	}
	user, err := os.ReadFile(filepath.Join(dir, home.FileUser))
	if err != nil {
		t.Fatal(err)
	}
	if home.IsTemplate(string(user)) || !strings.Contains(string(user), "李总") {
		t.Fatalf("user = %s", user)
	}
	if home.NeedsInit(dir) {
		t.Fatal("portrait was not persisted")
	}
}

func TestOwnerFollowUpSkipsScanWhenDenied(t *testing.T) {
	dir := t.TempDir()
	if err := home.Bootstrap(dir, "ou_me"); err != nil {
		t.Fatal(err)
	}
	coordinator, _, runner := homeCoordinator(t, dir, "ou_me")
	runner.reply = "好的，不会扫描。"
	coordinator.scanHome = t.TempDir()
	if err := os.MkdirAll(filepath.Join(coordinator.scanHome, ".codex", "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(coordinator.scanHome, ".codex", "sessions", "a.jsonl"), []byte(`{"role":"user","text":"secret project zebra"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Handle(t.Context(), Request{
		ConversationID: "dm", Input: "不允许扫描，叫我李总", SenderOpenID: "ou_me", ChatType: protocol.ChatP2P,
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(runner.seen()[0], "secret project zebra") {
		t.Fatal("scan ran after deny")
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
		ConversationID: "grp", Input: "hello", SenderOpenID: "ou_me", ChatType: protocol.ChatGroup, Mentioned: true,
	}); err != nil {
		t.Fatal(err)
	}
	if len(runner.seen()) == 0 || strings.Contains(runner.seen()[0], "private") || strings.Contains(runner.seen()[0], dir) {
		t.Fatalf("group leaked home: %v", runner.seen())
	}
	status, err := coordinator.Handle(t.Context(), Request{
		ConversationID: "grp", Input: "/status", SenderOpenID: "ou_me", ChatType: protocol.ChatGroup, Mentioned: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status.Text, "guest") || strings.Contains(status.Text, dir) {
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
		ConversationID: "grp", Input: "one", SenderOpenID: "ou_a", ChatType: protocol.ChatGroup, Mentioned: true,
	}); err != nil {
		t.Fatal(err)
	}
	hash := store.Conversation("grp").Sessions["codex"].CapabilityHash
	if _, err := coordinator.Handle(t.Context(), Request{
		ConversationID: "grp", Input: "two", SenderOpenID: "ou_b", ChatType: protocol.ChatGroup, Mentioned: true,
	}); err != nil {
		t.Fatal(err)
	}
	if store.Conversation("grp").Sessions["codex"].CapabilityHash != hash {
		t.Fatal("speaker line changed capability hash")
	}
	if len(runner.seen()) < 2 || !strings.Contains(runner.seen()[1], "speaker=ou_b") {
		t.Fatalf("second speaker missing: %v", runner.seen())
	}
}

func TestUnmentionedGroupAsksAgentToStaySilent(t *testing.T) {
	dir := t.TempDir()
	if err := home.Bootstrap(dir, "ou_me"); err != nil {
		t.Fatal(err)
	}
	coordinator, _, runner := homeCoordinator(t, dir, "ou_me")
	if _, err := coordinator.Handle(t.Context(), Request{
		ConversationID: "grp", Input: "随便聊聊", SenderOpenID: "ou_a", ChatType: protocol.ChatGroup,
	}); err != nil {
		t.Fatal(err)
	}
	if len(runner.seen()) == 0 || !strings.Contains(runner.seen()[0], home.ListenUnmentioned(home.LocaleZH)) {
		t.Fatalf("missing listen instruction: %v", runner.seen())
	}
}

func homeCoordinator(t *testing.T, homeDir, owner string) (*Coordinator, *state.Store, *fakeRunner) {
	t.Helper()
	coordinator, store, manager := homeCoordinatorWithManager(t, homeDir, owner)
	return coordinator, store, manager.runners["codex"]
}

func homeCoordinatorWithManager(t *testing.T, homeDir, owner string) (*Coordinator, *state.Store, *fakeManager) {
	t.Helper()
	catalog, err := agent.NewCatalog(map[string]agent.Config{
		"codex": {Harness: "codex", Default: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{id: "sess", reply: "请告诉我你常用的工作方式。"}
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": runner}}
	assembler := capability.NewAssembler(nil).SetHome(home.Dir{Path: homeDir})
	coordinator := newCoordinator(t, catalog, store, assembler, manager, time.Minute)
	coordinator.SetIdentity(owner, home.Dir{Path: homeDir})
	useHome(t, coordinator, homeDir)
	coordinator.scanHome = t.TempDir()
	return coordinator, store, manager
}
