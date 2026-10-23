package admin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/adapter"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/configbuild"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/desktop"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/runtime"
	"github.com/gopact-ai/steve/internal/skills"
)

// desktopAdminFixture is a first-launch desktop installation on a machine
// with pi and Claude Code installed. Their pinned adapters are already in
// the installation's adapter cache, so enrollment never reaches the network.
func desktopAdminFixture(t *testing.T) (*Service, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	for _, tool := range []string{"pi", "claude", "node", "npm"} {
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
	for _, name := range []string{"pi-acp", "claude-agent-acp"} {
		cachedAdapter(t, cfg.AdapterDir(), name, "#!/bin/sh\nexit 1\n")
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
	return &Service{NodeName: "node-hub", ConfigStore: NewConfigStore(cfg), Path: install.Paths.Config, Catalog: catalog, Manager: manager,
		LiveSkills: &skills.Live{Map: skillMap, After: func() error { t.Fatal("enrollment restarted existing agents"); return nil }}}, bin
}

// cachedAdapter places a verified installation of the pinned adapter name
// running script, the shape adapter.Installer leaves behind.
func cachedAdapter(t *testing.T, dir, name, script string) string {
	t.Helper()
	pin := adapter.Catalog[name]
	installed := filepath.Join(dir, name+"@"+pin.Version)
	command := filepath.Join(installed, pin.Bin)
	if err := os.MkdirAll(filepath.Dir(command), 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(command)
	if err := os.WriteFile(command, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	marker, _ := json.Marshal(map[string]string{"package": pin.Package, "version": pin.Version, "integrity": pin.Integrity})
	if err := os.WriteFile(filepath.Join(installed, ".steve-adapter.json"), marker, 0o600); err != nil {
		t.Fatal(err)
	}
	return command
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
	var pi consoleapi.DesktopAgentCandidate
	for _, candidate := range found.Agents {
		if candidate.ID == "pi" {
			pi = candidate
		}
	}
	if !pi.Installed || pi.Registered || pi.Executable != filepath.Join(bin, "pi") {
		t.Fatalf("discovered local agent: %+v", pi)
	}
	after, err := os.ReadFile(admin.Path)
	if err != nil || string(before) != string(after) || len(admin.Catalog.List()) != 0 {
		t.Fatal("discovery changed registration")
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(admin.cfg().Gateway.StatePath), "runtimes")); !os.IsNotExist(err) {
		t.Fatalf("discovery prepared tools: %v", err)
	}
}

func TestDesktopEnrollmentTakesTheNamesAndDefaultTheOwnerChose(t *testing.T) {
	admin, _ := desktopAdminFixture(t)
	status, err := admin.DesktopEnroll(t.Context(), consoleapi.DesktopEnrollRequest{Agents: []consoleapi.DesktopEnrollAgent{
		{CandidateID: "pi", AgentID: "reviewer", About: "代码评审"},
		{CandidateID: "claude", AgentID: "coder", Default: true},
	}})
	if err != nil || status.AgentCount != 2 || status.DefaultAgent != "coder" {
		t.Fatalf("enrollment result: %+v, %v", status, err)
	}
	reviewer, ok := admin.cfg().Agents["reviewer"]
	if !ok || reviewer.Harness != "pi" || reviewer.About != "代码评审" || reviewer.Default {
		t.Fatalf("named agent: %+v (present %v)", reviewer, ok)
	}
	if len(reviewer.Aliases) != 0 {
		t.Fatalf("a renamed agent kept the tool's own name as an alias: %v", reviewer.Aliases)
	}
	coder := admin.cfg().Agents["coder"]
	if coder.Harness != "claude-code" || !coder.Default {
		t.Fatalf("chosen default: %+v", coder)
	}
	if _, taken := admin.cfg().Agents["pi"]; taken {
		t.Fatal("the tool id was registered alongside the name the owner chose")
	}
	catalog, err := admin.cfg().AgentCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.List()) != 2 {
		t.Fatalf("catalog holds %d agents", len(catalog.List()))
	}
}

func TestDesktopEnrollmentRefusesNamesItCannotAnswerTo(t *testing.T) {
	admin, _ := desktopAdminFixture(t)
	for _, name := range []string{"Review Bot", "-pi", "pi/one"} {
		if _, err := admin.DesktopEnroll(t.Context(), consoleapi.DesktopEnrollRequest{Agents: []consoleapi.DesktopEnrollAgent{{CandidateID: "pi", AgentID: name}}}); err == nil {
			t.Fatalf("name %q was accepted", name)
		}
	}
	if len(admin.cfg().Agents) != 0 {
		t.Fatalf("a refused name still registered something: %v", admin.cfg().Agents)
	}
	if _, err := admin.DesktopEnroll(t.Context(), consoleapi.DesktopEnrollRequest{Agents: []consoleapi.DesktopEnrollAgent{
		{CandidateID: "pi", AgentID: "same"},
		{CandidateID: "claude", AgentID: "same"},
	}}); err == nil {
		t.Fatal("two agents were registered under one name")
	}
}

func TestDesktopEnrollmentPersistsOnlyChosenToolsAndReplaysSafely(t *testing.T) {
	admin, bin := desktopAdminFixture(t)
	status, err := admin.DesktopEnroll(t.Context(), consoleapi.DesktopEnrollRequest{AgentIDs: []string{"pi"}})
	if err != nil || !status.SetupRequired || status.AgentCount != 1 || status.DefaultAgent != "pi" {
		t.Fatalf("enrollment result: %+v, %v", status, err)
	}
	enrolled := admin.cfg().Harnesses["pi"]
	if enrolled.Adapter != "pi-acp" || !slices.Equal(enrolled.Env, []string{"PI_ACP_PI_COMMAND=" + filepath.Join(bin, "pi")}) || enrolled.Permission != config.PermissionRead {
		t.Fatalf("enrollment did not keep the selected pi and permission: %+v", enrolled)
	}
	stateDir := filepath.Dir(admin.cfg().Gateway.StatePath)
	for _, unselected := range []string{runtime.CodexHome(stateDir), runtime.ClaudeHome(stateDir)} {
		if _, err := os.Lstat(unselected); !os.IsNotExist(err) {
			t.Fatalf("unselected runtime prepared: %s, %v", unselected, err)
		}
	}
	if _, err := os.Stat(filepath.Join(runtime.PiHome(stateDir), "skills", "skill-creator", "SKILL.md")); err != nil {
		t.Fatalf("selected runtime did not receive enabled skills: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(runtime.PiHome(stateDir), "skills", "steve")); !os.IsNotExist(err) {
		t.Fatalf("platform MCP must not be installed as a skill: %v", err)
	}
	saved, err := config.Load(admin.Path)
	if err != nil || !reflect.DeepEqual(saved.Agents, admin.cfg().Agents) || len(saved.Harnesses) != 1 {
		t.Fatalf("restart config differs from enrolled state: %v", err)
	}
	admin.WriteConfig = func(string, *config.Config) error { t.Fatal("replayed enrollment rewrote config"); return nil }
	if _, err := admin.DesktopEnroll(t.Context(), consoleapi.DesktopEnrollRequest{AgentIDs: []string{"pi"}}); err != nil {
		t.Fatal(err)
	}
	admin.WriteConfig = nil
	status, err = admin.DesktopEnroll(t.Context(), consoleapi.DesktopEnrollRequest{AgentIDs: []string{"pi", "claude"}})
	if err != nil || status.AgentCount != 2 || status.DefaultAgent != "pi" {
		t.Fatalf("additional enrollment changed the default: %+v, %v", status, err)
	}
	found, err := admin.DesktopDiscover(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range found.Agents {
		if (candidate.ID == "pi" || candidate.ID == "claude") && !candidate.Registered {
			t.Fatalf("registered candidate is not marked: %+v", candidate)
		}
	}
}

func TestDesktopEnrollmentRespectsConfigCommitBoundary(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "before replacement", true: "after replacement"}[committed], func(t *testing.T) {
			admin, _ := desktopAdminFixture(t)
			admin.WriteConfig = func(path string, candidate *config.Config) error {
				if len(admin.cfg().Agents) != 0 || len(admin.Catalog.List()) != 0 {
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
			_, err := admin.DesktopEnroll(t.Context(), consoleapi.DesktopEnrollRequest{AgentIDs: []string{"pi"}})
			if err == nil || config.Committed(err) != committed {
				t.Fatalf("enrollment lost commit status: %v", err)
			}
			_, published := admin.Catalog.Resolve("pi")
			if published != committed || (len(admin.cfg().Harnesses) > 0) != committed {
				t.Fatal("published config disagrees with persistence")
			}
			if !committed {
				if _, err := admin.Manager.SupportsHTTPMCP(t.Context(), harness.Placement{Harness: "pi"}); err == nil || !strings.Contains(err.Error(), "unknown harness") {
					t.Fatalf("rejected runtime configuration was published: %v", err)
				}
			}
		})
	}
}

func TestDesktopEnrollmentRejectsUnrecognizedSelectionBeforeSideEffects(t *testing.T) {
	admin, _ := desktopAdminFixture(t)
	admin.WriteConfig = func(string, *config.Config) error { t.Fatal("invalid selection reached persistence"); return nil }
	if _, err := admin.DesktopEnroll(t.Context(), consoleapi.DesktopEnrollRequest{AgentIDs: []string{"pi", "/tmp/arbitrary-command"}}); err == nil {
		t.Fatal("invalid selection was accepted")
	}
	if len(admin.Catalog.List()) != 0 {
		t.Fatal("invalid batch partially enrolled a tool")
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(admin.cfg().Gateway.StatePath), "runtimes")); !os.IsNotExist(err) {
		t.Fatal("invalid batch prepared a runtime")
	}
}

func TestDesktopEnrollmentRejectsConflictingNamesBeforeRuntimePreparation(t *testing.T) {
	for _, conflict := range []string{"alias", "command"} {
		t.Run(conflict, func(t *testing.T) {
			admin, _ := desktopAdminFixture(t)
			if conflict == "alias" {
				admin.cfg().Agents["other"] = config.Agent{Harness: "custom", Aliases: []string{"pi"}, Default: true}
			} else {
				admin.cfg().Harnesses["pi"] = config.Harness{Command: "/custom-tool", Permission: config.PermissionRead}
			}
			if _, err := admin.DesktopEnroll(t.Context(), consoleapi.DesktopEnrollRequest{AgentIDs: []string{"pi"}}); err == nil {
				t.Fatal("conflicting configuration was accepted")
			}
			if _, err := os.Lstat(filepath.Join(filepath.Dir(admin.cfg().Gateway.StatePath), "runtimes")); !os.IsNotExist(err) {
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
	admin, _ := desktopAdminFixture(t)
	// The pinned adapter is the agent process; here it is the mock agent.
	cachedAdapter(t, admin.cfg().AdapterDir(), "pi-acp", "#!/bin/sh\nexec '"+bin+"' \"$@\"\n")
	if _, err := admin.DesktopEnroll(t.Context(), consoleapi.DesktopEnrollRequest{AgentIDs: []string{"pi"}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	runner, err := admin.Manager.OpenSession(ctx, harness.Placement{Harness: "pi"}, "", t.TempDir(), nil)
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
	for _, id := range []string{"pi", "claude"} {
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
		_, err := admin.DesktopEnroll(t.Context(), consoleapi.DesktopEnrollRequest{AgentIDs: []string{"pi"}})
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
	if _, ok := admin.Catalog.Resolve("pi"); !ok {
		t.Fatal("the saved agent was not published")
	}
}

// The running application learns the local workspace root without the
// configuration file being rewritten.
func TestSetLocalWorkspaceRootIsNotSaved(t *testing.T) {
	a := agentAdminFixture(t)
	before, err := os.ReadFile(a.Path)
	if err != nil {
		t.Fatal(err)
	}
	a.SetLocalWorkspaceRoot("/work/here")
	a.ConfigStore.Read(func(c *config.Config) {
		if c.Gateway.WorkspaceRoot != "/work/here" {
			t.Errorf("workspace root = %q", c.Gateway.WorkspaceRoot)
		}
	})
	after, err := os.ReadFile(a.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("recording the workspace root rewrote the configuration file")
	}
}
