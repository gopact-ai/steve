package admin

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/configbuild"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/desktop"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/runtime"
	"github.com/gopact-ai/steve/internal/skills"
)

func desktopAdminFixture(t *testing.T) (*Service, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	for _, tool := range []string{"grok", "kimi"} {
		if err := os.WriteFile(filepath.Join(bin, tool), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	install, err := desktop.Bootstrap(desktop.Options{StateDir: filepath.Join(t.TempDir(), "desktop")})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(install.Paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := cfg.AgentCatalog()
	if err != nil {
		t.Fatal(err)
	}
	manager, err := configbuild.HarnessManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Stop)
	skillMap, err := skills.Setup(filepath.Dir(cfg.Gateway.StatePath))
	if err != nil {
		t.Fatal(err)
	}
	return &Service{Cfg: cfg, Path: install.Paths.Config, Catalog: catalog, Manager: manager,
		LiveSkills: &skills.Live{Map: skillMap, After: func() error { t.Fatal("enrollment restarted existing agents"); return nil }}}, bin
}

func TestDesktopDiscoveryDoesNotEnrollOrPrepareTools(t *testing.T) {
	admin, bin := desktopAdminFixture(t)
	before, err := os.ReadFile(admin.Path)
	if err != nil {
		t.Fatal(err)
	}
	status, err := admin.DesktopStatus(t.Context())
	if err != nil || !status.Enabled || !status.SetupRequired || status.AgentCount != 0 || status.NodeID == "" {
		t.Fatalf("first-launch status: %+v, %v", status, err)
	}
	found, err := admin.DesktopDiscover(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var grok consoleapi.DesktopAgentCandidate
	for _, candidate := range found.Agents {
		if candidate.ID == "grok" {
			grok = candidate
		}
	}
	if !grok.Installed || grok.Registered || grok.Executable != filepath.Join(bin, "grok") {
		t.Fatalf("discovered local agent: %+v", grok)
	}
	after, err := os.ReadFile(admin.Path)
	if err != nil || string(before) != string(after) || len(admin.Catalog.List()) != 0 {
		t.Fatal("discovery changed registration")
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(admin.Cfg.Gateway.StatePath), "runtimes")); !os.IsNotExist(err) {
		t.Fatalf("discovery prepared tools: %v", err)
	}
}

func TestDesktopEnrollmentTakesTheNamesAndDefaultTheOwnerChose(t *testing.T) {
	admin, _ := desktopAdminFixture(t)
	status, err := admin.DesktopEnroll(t.Context(), consoleapi.DesktopEnrollRequest{Agents: []consoleapi.DesktopEnrollAgent{
		{CandidateID: "grok", AgentID: "reviewer", About: "代码评审"},
		{CandidateID: "kimi", AgentID: "coder", Default: true},
	}})
	if err != nil || status.AgentCount != 2 || status.DefaultAgent != "coder" {
		t.Fatalf("enrollment result: %+v, %v", status, err)
	}
	reviewer, ok := admin.Cfg.Agents["reviewer"]
	if !ok || reviewer.Harness != "grok" || reviewer.About != "代码评审" || reviewer.Default {
		t.Fatalf("named agent: %+v (present %v)", reviewer, ok)
	}
	if len(reviewer.Aliases) != 0 {
		t.Fatalf("a renamed agent kept the tool's own name as an alias: %v", reviewer.Aliases)
	}
	coder := admin.Cfg.Agents["coder"]
	if coder.Harness != "kimi" || !coder.Default {
		t.Fatalf("chosen default: %+v", coder)
	}
	if _, taken := admin.Cfg.Agents["grok"]; taken {
		t.Fatal("the tool id was registered alongside the name the owner chose")
	}
	catalog, err := admin.Cfg.AgentCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.List()) != 2 {
		t.Fatalf("catalog holds %d agents", len(catalog.List()))
	}
}

func TestDesktopEnrollmentRefusesNamesItCannotAnswerTo(t *testing.T) {
	admin, _ := desktopAdminFixture(t)
	for _, name := range []string{"Review Bot", "-grok", "grok/one"} {
		if _, err := admin.DesktopEnroll(t.Context(), consoleapi.DesktopEnrollRequest{Agents: []consoleapi.DesktopEnrollAgent{{CandidateID: "grok", AgentID: name}}}); err == nil {
			t.Fatalf("name %q was accepted", name)
		}
	}
	if len(admin.Cfg.Agents) != 0 {
		t.Fatalf("a refused name still registered something: %v", admin.Cfg.Agents)
	}
	if _, err := admin.DesktopEnroll(t.Context(), consoleapi.DesktopEnrollRequest{Agents: []consoleapi.DesktopEnrollAgent{
		{CandidateID: "grok", AgentID: "same"},
		{CandidateID: "kimi", AgentID: "same"},
	}}); err == nil {
		t.Fatal("two agents were registered under one name")
	}
}

func TestDesktopEnrollmentPersistsOnlyChosenToolsAndReplaysSafely(t *testing.T) {
	admin, bin := desktopAdminFixture(t)
	status, err := admin.DesktopEnroll(t.Context(), consoleapi.DesktopEnrollRequest{AgentIDs: []string{"grok"}})
	if err != nil || !status.SetupRequired || status.AgentCount != 1 || status.DefaultAgent != "grok" {
		t.Fatalf("enrollment result: %+v, %v", status, err)
	}
	if admin.Cfg.Harnesses["grok"].Command != filepath.Join(bin, "grok") || admin.Cfg.Harnesses["grok"].Permission != config.PermissionRead {
		t.Fatal("enrollment did not preserve the selected executable and permission")
	}
	stateDir := filepath.Dir(admin.Cfg.Gateway.StatePath)
	for _, unselected := range []string{runtime.CodexHome(stateDir), runtime.ClaudeHome(stateDir), runtime.KimiHome(stateDir)} {
		if _, err := os.Lstat(unselected); !os.IsNotExist(err) {
			t.Fatalf("unselected runtime prepared: %s, %v", unselected, err)
		}
	}
	if _, err := os.Stat(filepath.Join(runtime.GrokHome(stateDir), "skills", "skill-creator", "SKILL.md")); err != nil {
		t.Fatalf("selected runtime did not receive enabled skills: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(runtime.GrokHome(stateDir), "skills", "steve")); !os.IsNotExist(err) {
		t.Fatalf("platform MCP must not be installed as a skill: %v", err)
	}
	saved, err := config.Load(admin.Path)
	if err != nil || !reflect.DeepEqual(saved.Agents, admin.Cfg.Agents) || len(saved.Harnesses) != 1 {
		t.Fatalf("restart config differs from enrolled state: %v", err)
	}
	admin.WriteConfig = func(string, *config.Config) error { t.Fatal("replayed enrollment rewrote config"); return nil }
	if _, err := admin.DesktopEnroll(t.Context(), consoleapi.DesktopEnrollRequest{AgentIDs: []string{"grok"}}); err != nil {
		t.Fatal(err)
	}
	admin.WriteConfig = nil
	status, err = admin.DesktopEnroll(t.Context(), consoleapi.DesktopEnrollRequest{AgentIDs: []string{"grok", "kimi"}})
	if err != nil || status.AgentCount != 2 || status.DefaultAgent != "grok" {
		t.Fatalf("additional enrollment changed the default: %+v, %v", status, err)
	}
	found, err := admin.DesktopDiscover(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range found.Agents {
		if (candidate.ID == "grok" || candidate.ID == "kimi") && !candidate.Registered {
			t.Fatalf("registered candidate is not marked: %+v", candidate)
		}
	}
}

func TestDesktopEnrollmentRespectsConfigCommitBoundary(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "before replacement", true: "after replacement"}[committed], func(t *testing.T) {
			admin, _ := desktopAdminFixture(t)
			admin.WriteConfig = func(path string, candidate *config.Config) error {
				if len(admin.Cfg.Agents) != 0 || len(admin.Catalog.List()) != 0 {
					t.Fatal("enrollment was published before persistence")
				}
				if !committed {
					return errors.New("disk is full")
				}
				if err := config.Save(path, candidate); err != nil {
					return err
				}
				return &config.CommittedError{Err: errors.New("directory sync failed")}
			}
			_, err := admin.DesktopEnroll(t.Context(), consoleapi.DesktopEnrollRequest{AgentIDs: []string{"grok"}})
			if err == nil || config.Committed(err) != committed {
				t.Fatalf("enrollment lost commit status: %v", err)
			}
			_, published := admin.Catalog.Resolve("grok")
			if published != committed || (len(admin.Cfg.Harnesses) > 0) != committed {
				t.Fatal("published config disagrees with persistence")
			}
			if !committed {
				if _, err := admin.Manager.SupportsHTTPMCP(t.Context(), harness.Placement{Harness: "grok"}); err == nil || !strings.Contains(err.Error(), "unknown harness") {
					t.Fatalf("rejected runtime configuration was published: %v", err)
				}
			}
		})
	}
}

func TestDesktopEnrollmentRejectsUnrecognizedSelectionBeforeSideEffects(t *testing.T) {
	admin, _ := desktopAdminFixture(t)
	admin.WriteConfig = func(string, *config.Config) error { t.Fatal("invalid selection reached persistence"); return nil }
	if _, err := admin.DesktopEnroll(t.Context(), consoleapi.DesktopEnrollRequest{AgentIDs: []string{"grok", "/tmp/arbitrary-command"}}); err == nil {
		t.Fatal("invalid selection was accepted")
	}
	if len(admin.Catalog.List()) != 0 {
		t.Fatal("invalid batch partially enrolled a tool")
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(admin.Cfg.Gateway.StatePath), "runtimes")); !os.IsNotExist(err) {
		t.Fatal("invalid batch prepared a runtime")
	}
}

func TestDesktopEnrollmentRejectsConflictingNamesBeforeRuntimePreparation(t *testing.T) {
	for _, conflict := range []string{"alias", "command"} {
		t.Run(conflict, func(t *testing.T) {
			admin, _ := desktopAdminFixture(t)
			if conflict == "alias" {
				admin.Cfg.Agents["other"] = config.Agent{Harness: "custom", Aliases: []string{"grok"}, Default: true}
			} else {
				admin.Cfg.Harnesses["grok"] = config.Harness{Command: "/custom-tool", Permission: config.PermissionRead}
			}
			if _, err := admin.DesktopEnroll(t.Context(), consoleapi.DesktopEnrollRequest{AgentIDs: []string{"grok"}}); err == nil {
				t.Fatal("conflicting configuration was accepted")
			}
			if _, err := os.Lstat(filepath.Join(filepath.Dir(admin.Cfg.Gateway.StatePath), "runtimes")); !os.IsNotExist(err) {
				t.Fatal("conflicting registration prepared a runtime before validation")
			}
		})
	}
}

func TestDesktopEnrollmentRunsSelectedAgentWithoutRestart(t *testing.T) {
	// Build before the fixture isolates PATH and HOME.
	bin := filepath.Join(t.TempDir(), "mockagent")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build mockagent: %v\n%s", err, output)
	}
	admin, toolsDir := desktopAdminFixture(t)
	if err := os.Remove(filepath.Join(toolsDir, "grok")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(bin, filepath.Join(toolsDir, "grok")); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.DesktopEnroll(t.Context(), consoleapi.DesktopEnrollRequest{AgentIDs: []string{"grok"}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	runner, err := admin.Manager.OpenSession(ctx, harness.Placement{Harness: "grok"}, "", t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	text, _, err := runner.Prompt(ctx, "hello", nil)
	if err != nil || !strings.Contains(text, "hello") {
		t.Fatalf("selected agent prompt: %q, %v", text, err)
	}
}

func TestDesktopConcurrentRegistrationAndReadsRetainBothAgents(t *testing.T) {
	admin, _ := desktopAdminFixture(t)
	var workers sync.WaitGroup
	errorsFound := make(chan error, 6)
	for _, id := range []string{"grok", "kimi"} {
		workers.Go(func() {
			_, err := admin.DesktopEnroll(t.Context(), consoleapi.DesktopEnrollRequest{AgentIDs: []string{id}})
			if err != nil {
				errorsFound <- err
			}
		})
	}
	for range 4 {
		workers.Go(func() {
			for range 8 {
				if _, err := admin.DesktopStatus(t.Context()); err != nil {
					errorsFound <- err
					return
				}
				if _, err := admin.DesktopDiscover(t.Context()); err != nil {
					errorsFound <- err
					return
				}
			}
		})
	}
	workers.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Error(err)
	}
	status, err := admin.DesktopStatus(t.Context())
	if err != nil || status.AgentCount != 2 || status.DefaultAgent == "" {
		t.Fatalf("concurrent registrations lost an Agent: %+v, %v", status, err)
	}
	saved, err := config.Load(admin.Path)
	if err != nil || len(saved.Agents) != 2 {
		t.Fatalf("concurrent registrations lost persisted Agent: %v", err)
	}
}

// The desktop status can be read while newly chosen agents are being saved.
func TestDesktopStatusReadDuringAnEnrollmentSave(t *testing.T) {
	admin, _ := desktopAdminFixture(t)
	var during consoleapi.DesktopStatus
	finished := readsDuringSave(t, admin, func() error {
		_, err := admin.DesktopEnroll(t.Context(), consoleapi.DesktopEnrollRequest{AgentIDs: []string{"grok"}})
		return err
	}, func() error {
		var err error
		during, err = admin.DesktopStatus(t.Context())
		return err
	})
	if !finished {
		t.Fatal("reading the desktop status waited for an enrollment save")
	}
	if during.AgentCount != 0 {
		t.Fatal("a reader saw agents that were not saved yet")
	}
	if _, ok := admin.Catalog.Resolve("grok"); !ok {
		t.Fatal("the saved agent was not published")
	}
}
