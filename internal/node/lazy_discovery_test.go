package node

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ability"
)

func lazyCounterProgram(t *testing.T, name string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	program, counter := filepath.Join(dir, name), filepath.Join(dir, "starts")
	body := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + strings.ReplaceAll(counter, "'", "'\\''") + "'\necho 'fixture 1.2.3'\n"
	if err := os.WriteFile(program, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return program, counter
}
func lazyStarts(t *testing.T, counter string) int {
	t.Helper()
	raw, err := os.ReadFile(counter)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(raw), "\n")
}
func lazyWait(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("background host tool pass did not complete")
}

// Serve and a real settings write exercise both background hookups. The fixture
// programs record every process start, not just the commands list's shape.
func TestLazyNodeStartupAndSettingsWakeNeverLaunchConfiguredAgents(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, count := range []int{100, 1000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			program, starts := lazyCounterProgram(t, "custom-acp")
			tool, toolStarts := lazyCounterProgram(t, "host-tool")
			t.Setenv("PATH", filepath.Dir(tool)+string(os.PathListSeparator)+os.Getenv("PATH"))
			cfg := ServerConfig{Name: "fixture-node", Token: "fixture-only", Source: filepath.Join(t.TempDir(), "node.json"), StateDir: t.TempDir(), WorkspaceRoot: t.TempDir(), Tools: []string{filepath.Base(tool)}, Harnesses: map[string]HarnessSpec{}}
			for i := range count {
				cfg.Harnesses[fmt.Sprintf("custom-%04d", i)] = HarnessSpec{Command: program, Args: []string{"acp", "", "含 空格"}, Env: []string{"MODE=ordinary"}}
			}
			for _, id := range []string{"codex", "claude-code", "grok", "kimi"} {
				cfg.Harnesses[id] = HarnessSpec{Command: program, Adapter: "codex-acp"}
			}
			if err := writeConfig(cfg); err != nil {
				t.Fatal(err)
			}
			server := startNode(t, cfg)
			lazyWait(t, func() bool { _, ok := server.launch.Lookup(tool); return ok })
			registry := NewRegistry("fixture-hub", map[string]Config{cfg.Name: {Addr: server.Addr(), Token: cfg.Token}})
			t.Cleanup(registry.Close)
			for range 3 {
				current, err := registry.Settings(t.Context(), cfg.Name)
				if err != nil {
					t.Fatal(err)
				}
				if len(current.Harnesses) != count+4 {
					t.Fatal("read lost configured harnesses")
				}
				if _, err := registry.AgentTools(t.Context(), cfg.Name); err != nil {
					t.Fatal(err)
				}
				if _, err := registry.Refresh(t.Context(), cfg.Name); err != nil {
					t.Fatal(err)
				}
			}
			for i := range 2 {
				set := server.settings()
				set.Capabilities = []string{fmt.Sprintf("wake-%d", i)}
				before, _ := server.launch.Lookup(tool)
				if err := server.applySettings(set); err != nil {
					t.Fatal(err)
				}
				lazyWait(t, func() bool { after, ok := server.launch.Lookup(tool); return ok && after.At.After(before.At) })
			}
			if got := lazyStarts(t, starts); got != 0 {
				t.Errorf("%d configured harnesses started %d agent processes without a job or explicit validation", count, got)
			}
			if got := lazyStarts(t, toolStarts); got < 3 {
				t.Errorf("ordinary host tool was not checked on startup/wake: %d", got)
			}
			if _, ok := server.launch.Lookup(program); ok {
				t.Error("agent acquired bare-executable launch evidence")
			}
		})
	}
}

func TestLazyNodeSharedExecutableAndAliasesRemainPathOnly(t *testing.T) {
	program, starts := lazyCounterProgram(t, "shared-program")
	tool, toolStarts := lazyCounterProgram(t, "ordinary-tool")
	alias := filepath.Join(t.TempDir(), "program-alias")
	if err := os.Symlink(program, alias); err != nil {
		t.Fatal(err)
	}
	hardAlias := filepath.Join(t.TempDir(), "hard-alias")
	if err := os.Link(program, hardAlias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(program)+string(os.PathListSeparator)+os.Getenv("PATH"))
	server := NewServer(ServerConfig{Harnesses: map[string]HarnessSpec{"custom": {Command: filepath.Base(program), Args: []string{"acp"}, Env: []string{"MODE=fixture"}}}, Tools: []string{program, alias, hardAlias, tool, tool}})
	server.launch.pass(t.Context(), server.commands())
	if got := lazyStarts(t, starts); got != 0 {
		t.Errorf("host-tool overlap started the agent %d times", got)
	}
	if got := lazyStarts(t, toolStarts); got != 1 {
		t.Errorf("ordinary host tool starts = %d, want 1", got)
	}
}

func TestLazySnapshotDiscardsUnsafeCachedAgentLaunchEvidence(t *testing.T) {
	program, starts := lazyCounterProgram(t, "formerly-probed-agent")
	t.Setenv("PATH", filepath.Dir(program)+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, good := range []bool{true, false} {
		t.Run(fmt.Sprint(good), func(t *testing.T) {
			if !good {
				raw, err := os.ReadFile(program)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(program, append(raw, []byte("exit 126\n")...), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			probe := NewLaunchProbe()
			probe.pass(context.Background(), []string{program}) // Real old-policy evidence.
			prior, ok := probe.Lookup(program)
			if !ok {
				t.Fatal("missing old-policy evidence")
			}
			if prior.OK != good {
				t.Fatalf("old-policy launch = %+v; want OK=%v", prior, good)
			}
			before := lazyStarts(t, starts)
			snapshot := Snapshot("fixture", 1, 1, Observe{Harnesses: map[string]HarnessSpec{"custom": {Command: program, Args: []string{"acp"}, Env: []string{"MODE=ordinary"}}}, Tools: []string{filepath.Base(program)}, MCP: map[string]MCPSpec{"server": {Command: program}}, Launch: probe.Lookup})
			if snapshot == nil {
				t.Fatal("missing snapshot")
			}
			for _, capability := range snapshot.Offers {
				if capability.Kind != ability.Harness && capability.Kind != ability.Tool && capability.Kind != ability.MCP {
					continue
				}
				if capability.Assurance != ability.Existence || capability.Version != nil || len(capability.Evidence) != 1 || capability.Evidence[0].Method != "path" || capability.Availability != ability.Available {
					t.Errorf("unsafe cached launch projected as protocol/launch readiness: %+v", capability)
				}
			}
			if lazyStarts(t, starts) != before {
				t.Error("availability read executed an agent")
			}
			missing := Snapshot("fixture", 1, 2, Observe{Harnesses: map[string]HarnessSpec{"missing": {Command: "lazy-fixture-absent-agent"}}, Launch: probe.Lookup})
			if missing == nil {
				t.Fatal("missing PATH observation")
			}
			for _, capability := range missing.Offers {
				if capability.Kind == ability.Harness && capability.Availability != ability.Unavailable {
					t.Errorf("missing executable was available: %+v", capability)
				}
			}
		})
	}
}
