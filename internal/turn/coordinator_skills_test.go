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
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/skills"
	"github.com/gopact-ai/steve/internal/state"
)

func TestOwnerSkillsEnableDisableAndDrift(t *testing.T) {
	coordinator, store, live, dest := skillsCoordinator(t)
	list, err := ownerHandle(t, coordinator, "/skills")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(list.Text, "remind (off)") || !strings.Contains(list.Text, "enabled: (none)") {
		t.Fatalf("list = %s", list.Text)
	}
	enable, err := ownerHandle(t, coordinator, "/skills enable remind")
	if err != nil || !strings.Contains(enable.Text, i18n.New(i18n.LocaleZH).T(i18n.SkillsEnabled, "remind", protocol.CommandNew)) {
		t.Fatalf("enable = %#v, %v", enable, err)
	}
	if live.restarts != 1 {
		t.Fatalf("restarts = %d", live.restarts)
	}
	if _, err := os.Readlink(filepath.Join(dest, "remind")); err != nil {
		t.Fatal(err)
	}
	if _, err := ownerHandle(t, coordinator, "hello"); err != nil {
		t.Fatal(err)
	}
	if _, err := ownerHandle(t, coordinator, "/skills disable remind"); err != nil {
		t.Fatal(err)
	}
	if live.restarts != 2 {
		t.Fatalf("restarts after disable = %d", live.restarts)
	}
	_, err = ownerHandle(t, coordinator, "again")
	if err == nil || !strings.Contains(err.Error(), "/new") {
		t.Fatalf("expected capability drift after disable, got %v", err)
	}
	if _, ok := store.Conversation("dm").Sessions["codex"]; !ok {
		t.Fatal("session should remain until /new")
	}
}

func TestGuestCannotManageSkills(t *testing.T) {
	coordinator, _, _, _ := skillsCoordinator(t)
	result, err := coordinator.Handle(t.Context(), Request{
		ConversationID: "grp", Input: "/skills enable remind", SenderOpenID: "ou_me", ChatType: protocol.ChatGroup,
	})
	if err != nil || !strings.Contains(result.Text, i18n.New(i18n.LocaleZH).T(i18n.SkillsOwnerOnly)) {
		t.Fatalf("guest = %#v, %v", result, err)
	}
}

func TestSkillsBlockedDuringTurn(t *testing.T) {
	coordinator, _, live, _ := skillsCoordinator(t)
	runner := live.runner
	runner.started = make(chan struct{})
	runner.done = make(chan struct{})
	turnDone := make(chan error, 1)
	go func() {
		_, err := ownerHandle(t, coordinator, "long")
		turnDone <- err
	}()
	select {
	case <-runner.started:
	case <-time.After(waitDeadline):
		t.Fatal("turn did not start")
	}
	result, err := ownerHandle(t, coordinator, "/skills enable remind")
	if err != nil || !strings.Contains(result.Text, i18n.New(i18n.LocaleZH).T(i18n.SkillsBusy, protocol.CommandCancel)) {
		t.Fatalf("busy = %#v, %v", result, err)
	}
	if live.restarts != 0 {
		t.Fatal("mutated skills during a turn")
	}
	close(runner.done)
	if err := <-turnDone; err != nil {
		t.Fatal(err)
	}
}

func TestSkillsEnableAbsolutePath(t *testing.T) {
	coordinator, _, live, dest := skillsCoordinator(t)
	outside := filepath.Join(t.TempDir(), "lark-im")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "SKILL.md"), []byte("im"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := ownerHandle(t, coordinator, "/skills enable "+outside)
	if err != nil || !strings.Contains(result.Text, i18n.New(i18n.LocaleZH).T(i18n.SkillsEnabled, outside, protocol.CommandNew)) {
		t.Fatalf("enable path = %#v, %v", result, err)
	}
	if _, err := os.Readlink(filepath.Join(dest, "lark-im")); err != nil {
		t.Fatal(err)
	}
	status, err := ownerHandle(t, coordinator, "/status")
	if err != nil || !strings.Contains(status.Text, "lark-im") {
		t.Fatalf("status = %#v, %v", status, err)
	}
	if live.restarts != 1 {
		t.Fatalf("restarts = %d", live.restarts)
	}
}

func TestSkillsPathAddThenEnableByName(t *testing.T) {
	coordinator, _, _, _ := skillsCoordinator(t)
	extra := t.TempDir()
	if err := os.MkdirAll(filepath.Join(extra, "weather"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extra, "weather", "SKILL.md"), []byte("w"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ownerHandle(t, coordinator, "/skills path add "+extra); err != nil {
		t.Fatal(err)
	}
	result, err := ownerHandle(t, coordinator, "/skills enable weather")
	if err != nil || !strings.Contains(result.Text, i18n.New(i18n.LocaleZH).T(i18n.SkillsEnabled, "weather", protocol.CommandNew)) {
		t.Fatalf("enable after path add = %#v, %v", result, err)
	}
}

type liveCounter struct {
	*skills.Live
	restarts int
	runner   *fakeRunner
}

func skillsCoordinator(t *testing.T) (*Coordinator, *state.Store, *liveCounter, string) {
	t.Helper()
	root := t.TempDir()
	homeDir := filepath.Join(root, "home")
	if err := home.Bootstrap(homeDir, "ou_me"); err != nil {
		t.Fatal(err)
	}
	search := filepath.Join(root, "catalog")
	if err := os.MkdirAll(filepath.Join(search, "remind"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(search, "remind", "SKILL.md"), []byte("remind"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := skills.Open(filepath.Join(root, "skills.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Ensure(search); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(root, "runtime")
	counter := &liveCounter{runner: &fakeRunner{id: "sess"}}
	live := &skills.Live{
		Map:   m,
		Dests: []string{dest},
		After: func() error {
			counter.restarts++
			return nil
		},
	}
	counter.Live = live
	catalog, err := agent.NewCatalog(map[string]agent.Config{
		"codex": {Harness: "codex", Workspace: t.TempDir(), Default: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(filepath.Join(root, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": counter.runner}}
	assembler := capability.NewAssembler(nil).SetHome(home.Dir{Path: homeDir}).SetSkills(m)
	coordinator := New(catalog, store, assembler, manager, time.Minute)
	coordinator.SetIdentity("ou_me", home.Dir{Path: homeDir})
	coordinator.SetSkills(live)
	return coordinator, store, counter, dest
}

func ownerHandle(t *testing.T, c *Coordinator, input string) (Result, error) {
	t.Helper()
	return c.Handle(t.Context(), Request{
		ConversationID: "dm", Input: input, SenderOpenID: "ou_me", ChatType: protocol.ChatP2P,
	})
}
