package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/roster"
)

func harnessRuntimeFixture(t *testing.T, id string, env []string) (*config.Config, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		Gateway:   config.Gateway{StatePath: filepath.Join(dir, "state", "state.json")},
		Harnesses: map[string]config.Harness{id: {Command: "mock", Env: env}},
		Agents:    map[string]config.Agent{"primary": {Harness: id, Default: true}},
		Projects:  map[string]config.Project{"project": {Home: config.ProjectHome{Path: filepath.Join(dir, "project")}}},
	}
	path := filepath.Join(dir, "config.json")
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return loaded, path
}

func TestHarnessRuntimeEnvSurvivesPolicySaveAndStateMove(t *testing.T) {
	for _, tc := range []struct{ id, key string }{
		{harness.Codex, harness.EnvCodexHome},
		{harness.ClaudeCode, harness.EnvClaudeConfigDir},
		{harness.Grok, harness.EnvGrokHome},
		{harness.Kimi, harness.EnvKimiCodeHome},
	} {
		for _, declaration := range []struct {
			name string
			env  []string
		}{
			{"empty", nil},
			{"unrelated", []string{"USER_OPTION=kept"}},
			{"override", []string{"USER_OPTION=kept", tc.key + "=/custom/home"}},
		} {
			t.Run(tc.id+"/"+declaration.name, func(t *testing.T) {
				cfg, path := harnessRuntimeFixture(t, tc.id, declaration.env)
				wantEnv := slices.Clone(cfg.Harnesses[tc.id].Env)
				checkRuntime := func(c *config.Config) {
					t.Helper()
					wantHome := filepath.Join(filepath.Dir(c.Gateway.StatePath), "runtimes", tc.id)
					if declaration.name == "override" {
						wantHome = "/custom/home"
					}
					running := harnessRuntimeConfig(c)
					if !slices.Contains(running.Harnesses[tc.id].Env, tc.key+"="+wantHome) {
						t.Errorf("runtime Env = %v; want %s=%s", running.Harnesses[tc.id].Env, tc.key, wantHome)
					}
					if !reflect.DeepEqual(c.Harnesses[tc.id].Env, wantEnv) {
						t.Errorf("runtime derivation changed declared Env: %v", c.Harnesses[tc.id].Env)
					}
				}
				checkRuntime(cfg)
				patched, err := cfg.PatchSettings(json.RawMessage(`{"policies":{"execution":{"step_timeout":"22m"}}}`))
				if err != nil {
					t.Fatal(err)
				}
				if err := config.Save(path, patched); err != nil {
					t.Fatal(err)
				}
				reloaded, err := config.Load(path)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(reloaded.Harnesses[tc.id].Env, wantEnv) {
					t.Errorf("unrelated policy save persisted runtime Env: %v", reloaded.Harnesses[tc.id].Env)
				}
				if time.Duration(reloaded.Policies.Execution.StepTimeout) != 22*time.Minute {
					t.Fatal("policy update was not saved")
				}
				reloaded.Gateway.StatePath = filepath.Join(t.TempDir(), "moved", "state.json")
				if err := config.Save(path, reloaded); err != nil {
					t.Fatal(err)
				}
				moved, err := config.Load(path)
				if err != nil {
					t.Fatal(err)
				}
				checkRuntime(moved)
			})
		}
	}
}

func TestHarnessRuntimeConfigPreservesPreparedAdapter(t *testing.T) {
	cfg, path := harnessRuntimeFixture(t, harness.Codex, []string{"CODEX_HOME=/custom/home"})
	h := cfg.Harnesses[harness.Codex]
	h.Adapter, h.Command = "codex-acp", "resolved-adapter"
	cfg.Harnesses[harness.Codex] = h
	running := harnessRuntimeConfig(cfg)
	if running.Harnesses[harness.Codex].Command != "resolved-adapter" {
		t.Fatal("runtime lost prepared adapter command")
	}
	running.Harnesses[harness.Codex].Env[0] = "CODEX_HOME=/different"
	if cfg.Harnesses[harness.Codex].Env[0] != "CODEX_HOME=/custom/home" {
		t.Error("runtime Env aliases the declared configuration")
	}
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Harnesses[harness.Codex].Command != "" || reloaded.Harnesses[harness.Codex].Adapter != "codex-acp" || cfg.Harnesses[harness.Codex].Command != "resolved-adapter" {
		t.Fatal("prepared adapter command persistence changed")
	}
}

func TestHubNodeSettingsDerivesEnvOnlyForRunningHarness(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  []string
	}{
		{"empty", nil},
		{"override", []string{"CODEX_HOME=/custom/home"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, path := harnessRuntimeFixture(t, harness.Codex, tc.env)
			output := filepath.Join(t.TempDir(), "runtime-home")
			h := cfg.Harnesses[harness.Codex]
			h.Command = "/bin/sh"
			h.Args = []string{"-c", `printf '%s' "$CODEX_HOME" > "$1"`, "env-probe", output}
			cfg.Harnesses[harness.Codex] = h
			if err := config.Save(path, cfg); err != nil {
				t.Fatal(err)
			}
			catalog, err := cfg.AgentCatalog()
			if err != nil {
				t.Fatal(err)
			}
			manager, err := harnessRuntimeConfig(cfg).HarnessManager()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(manager.Stop)
			a := &fleetAdmin{cfg: cfg, path: path, catalog: catalog, manager: manager, assembler: cfg.CapabilityAssembler(), fleet: roster.New(catalog)}
			set, err := a.NodeSettings(t.Context(), nodeName())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(set.Harnesses[harness.Codex].Env, tc.env) {
				t.Errorf("settings GET exposes runtime Env: %v", set.Harnesses[harness.Codex].Env)
			}
			set.Tools = []string{"git"}
			if _, err := a.SetNodeSettings(t.Context(), nodeName(), set); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			// The probe writes its environment and exits without speaking ACP.
			if _, err := manager.SupportsHTTPMCP(ctx, harness.Placement{Harness: harness.Codex}); err == nil {
				t.Fatal("environment probe unexpectedly spoke ACP")
			}
			raw, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			wantHome := filepath.Join(filepath.Dir(cfg.Gateway.StatePath), "runtimes", harness.Codex)
			if tc.name == "override" {
				wantHome = "/custom/home"
			}
			if string(raw) != wantHome {
				t.Errorf("running harness home = %q; want %q", raw, wantHome)
			}
			reloaded, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(reloaded.Harnesses[harness.Codex].Env, tc.env) || !reflect.DeepEqual(cfg.Harnesses[harness.Codex].Env, tc.env) {
				t.Fatal("node settings save persisted derived runtime Env")
			}
		})
	}
}
