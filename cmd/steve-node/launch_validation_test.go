package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gopact-ai/steve/internal/node"
)

func TestLaunchCLILoadRejectsInvalidStartup(t *testing.T) {
	for _, tc := range []struct {
		name, command string
		args, env     []string
	}{
		{"empty command", "", nil, nil},
		{"blank command", "  ", nil, nil},
		{"command NUL", "unknown\x00agent", nil, nil},
		{"arg NUL", "unknown-agent", []string{"ok", "bad\x00arg"}, nil},
		{"missing equals", "unknown-agent", nil, []string{"MODE"}},
		{"empty key", "unknown-agent", nil, []string{"=value"}},
		{"bad key", "unknown-agent", nil, []string{"BAD-KEY=value"}},
		{"digit key", "unknown-agent", nil, []string{"1MODE=value"}},
		{"unicode key", "unknown-agent", nil, []string{"模式=value"}},
		{"env NUL", "unknown-agent", nil, []string{"MODE=bad\x00value"}},
		{"duplicate key", "unknown-agent", nil, []string{"MODE=one", "MODE=two"}},
		{"internal mark", "unknown-agent", nil, []string{"STEVE_PROCESS_MARK=override"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := node.ServerConfig{Name: "test", Token: "fixture-only", Harnesses: map[string]node.HarnessSpec{"custom": {Command: tc.command, Args: tc.args, Env: tc.env}}}
			raw, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "node.json")
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := load(path); err == nil {
				t.Fatal("invalid launch accepted")
			}
		})
	}
}

func TestLaunchCLISaveReloadPreservesStartup(t *testing.T) {
	want := node.HarnessSpec{Command: "未知 tool", Args: []string{"", "中文", "two words", `quoted"arg`, "$HOME", "--unknown"}, Env: []string{"MODE=", "_CUSTOM=中文 value=tail", "Path=one", "PATH=two"}, ProcessDir: t.TempDir()}
	cfg := node.ServerConfig{Name: "test", Token: "fixture-only", StateDir: t.TempDir(), Harnesses: map[string]node.HarnessSpec{"custom": want, "pinned": {Adapter: "codex-acp", Env: []string{"MODE="}}}}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "node.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := load(path)
	if err != nil {
		t.Fatal(err)
	}
	got.Source = path
	pin := got.Harnesses["pinned"]
	pin.Command = "fixture-resolved-adapter"
	got.Harnesses["pinned"] = pin
	// This public configuration operation uses the same node file writer as
	// online settings; no listener, broker or agent is started.
	server := node.NewServer(got)
	if err := server.SetWorkspaceRoot(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	reloaded, err := load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reloaded.Harnesses["custom"], want) {
		t.Fatal("node save/CLI reload changed command/argv/env")
	}
	persistedPin := reloaded.Harnesses["pinned"]
	if persistedPin.Command != "" || persistedPin.Adapter != pin.Adapter || !reflect.DeepEqual(persistedPin.Env, pin.Env) || len(persistedPin.Args) != 0 {
		t.Fatal("node save/CLI reload lost adapter declaration")
	}
}

func TestLaunchCLILoadAdapterContract(t *testing.T) {
	for _, tc := range []struct {
		name  string
		h     node.HarnessSpec
		valid bool
	}{
		{"pin", node.HarnessSpec{Adapter: "codex-acp", Env: []string{"MODE="}}, true},
		{"neither", node.HarnessSpec{}, false},
		{"both", node.HarnessSpec{Adapter: "codex-acp", Command: "unknown-agent"}, false},
		{"unknown adapter", node.HarnessSpec{Adapter: "unknown-acp"}, false},
		{"empty arg", node.HarnessSpec{Adapter: "codex-acp", Args: []string{""}}, false},
		{"pin duplicate env", node.HarnessSpec{Adapter: "codex-acp", Env: []string{"MODE=one", "MODE=two"}}, false},
		{"pin reserved env", node.HarnessSpec{Adapter: "codex-acp", Env: []string{"STEVE_PROCESS_MARK=override"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(node.ServerConfig{Name: "test", Token: "fixture-only", Harnesses: map[string]node.HarnessSpec{"custom": tc.h}})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "node.json")
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			_, err = load(path)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}
