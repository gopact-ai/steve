package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func launchFixture(t *testing.T, h Harness) *Config {
	t.Helper()
	return &Config{Harnesses: map[string]Harness{"custom": h}, Projects: map[string]Project{"work": {Home: ProjectHome{Path: t.TempDir()}}}, Gateway: Gateway{StatePath: filepath.Join(t.TempDir(), "state.json")}}
}

func TestLaunchLoadRejectsInvalidStartup(t *testing.T) {
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
			cfg := launchFixture(t, Harness{Command: tc.command, Args: tc.args, Env: tc.env})
			raw, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("invalid launch accepted")
			}
		})
	}
}

func TestLaunchSaveReloadPreservesStartup(t *testing.T) {
	want := Harness{Command: "未知 tool", Args: []string{"", "中文", "two words", `quoted"arg`, "$HOME", "--unknown"}, Env: []string{"MODE=", "_CUSTOM=中文 value=tail", "Path=one", "PATH=two"}, Permission: PermissionRead}
	cfg := launchFixture(t, want)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Harnesses["custom"], want) {
		t.Fatalf("startup changed: %#v", got.Harnesses["custom"])
	}
}

func TestLaunchLoadAdapterEnvContract(t *testing.T) {
	for _, env := range [][]string{{"MODE=one", "MODE=two"}, {"STEVE_PROCESS_MARK=override"}, {"BAD-KEY=value"}} {
		cfg := launchFixture(t, Harness{Adapter: "codex-acp", Env: env})
		raw, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatal("adapter accepted invalid environment")
		}
	}
}
