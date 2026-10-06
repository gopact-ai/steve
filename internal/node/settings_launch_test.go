package node

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gopact-ai/steve/internal/nodewire"
)

func launchSettingsFixture(t *testing.T) *Server {
	t.Helper()
	cfg := ServerConfig{Source: filepath.Join(t.TempDir(), "node.json"), Name: "test", Token: "fixture-only", StateDir: t.TempDir(), Harnesses: map[string]HarnessSpec{"custom": {Command: "unknown-agent"}}}
	if err := writeConfig(cfg); err != nil {
		t.Fatal(err)
	}
	return NewServer(cfg)
}

func TestLaunchNodeSettingsRejectsInvalidStartup(t *testing.T) {
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
			s := launchSettingsFixture(t)
			before, err := os.ReadFile(s.conf().Source)
			if err != nil {
				t.Fatal(err)
			}
			set := s.settings()
			revision := set.Revision
			set.Harnesses["custom"] = nodewire.HarnessSetting{Command: tc.command, Args: tc.args, Env: tc.env}
			if err := s.applySettings(set); err == nil {
				t.Fatal("invalid launch accepted")
			}
			after, err := os.ReadFile(s.conf().Source)
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) || s.settings().Revision != revision {
				t.Fatal("rejected launch changed file or runtime")
			}
		})
	}
}

func TestLaunchNodeSettingsSaveReloadPreservesStartup(t *testing.T) {
	s := launchSettingsFixture(t)
	set := s.settings()
	want := HarnessSpec{Command: "未知 tool", Args: []string{"", "中文", "two words", `quoted"arg`, "$HOME", "--unknown"}, Env: []string{"MODE=", "_CUSTOM=中文 value=tail", "Path=one", "PATH=two"}, ProcessDir: t.TempDir()}
	set.Harnesses["custom"] = nodewire.HarnessSetting{Command: want.Command, Args: want.Args, Env: want.Env, ProcessDir: want.ProcessDir}
	if err := s.applySettings(set); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(s.conf().Source)
	if err != nil {
		t.Fatal(err)
	}
	var cfg ServerConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	reloaded := NewServer(cfg)
	if !reflect.DeepEqual(cfg.Harnesses["custom"], want) || !reflect.DeepEqual(reloaded.settings().Harnesses["custom"].Args, want.Args) || !reflect.DeepEqual(s.conf().Harnesses["custom"], want) {
		t.Fatal("saved startup differs from runtime/reloaded settings")
	}
}

func TestLaunchNodeSettingsPinnedAdapterRejectsArgs(t *testing.T) {
	s := settingsFixture(t)
	before, err := os.ReadFile(s.conf().Source)
	if err != nil {
		t.Fatal(err)
	}
	set := s.settings()
	revision := set.Revision
	h := set.Harnesses["mock"]
	h.Args = []string{""}
	set.Harnesses["mock"] = h
	if err := s.applySettings(set); err == nil {
		t.Fatal("pinned adapter accepted custom argv")
	}
	after, err := os.ReadFile(s.conf().Source)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) || s.settings().Revision != revision {
		t.Fatal("rejected adapter edit changed file or runtime")
	}
}

func TestLaunchNodeSettingsValidatesRetainedEnv(t *testing.T) {
	for _, env := range [][]string{{"MODE=one", "MODE=two"}, {"STEVE_PROCESS_MARK=override"}} {
		cfg := ServerConfig{Source: filepath.Join(t.TempDir(), "node.json"), Name: "test", Token: "fixture-only", Harnesses: map[string]HarnessSpec{"custom": {Adapter: "codex-acp", Command: "fixture-adapter", Env: env}}}
		if err := writeConfig(cfg); err != nil {
			t.Fatal(err)
		}
		s := NewServer(cfg)
		before, err := os.ReadFile(cfg.Source)
		if err != nil {
			t.Fatal(err)
		}
		set := s.settings()
		revision := set.Revision
		h := set.Harnesses["custom"]
		h.Env = nil // Omission retains the old environment, so validate it too.
		set.Harnesses["custom"] = h
		if err := s.applySettings(set); err == nil {
			t.Fatal("invalid retained adapter environment accepted")
		}
		after, err := os.ReadFile(cfg.Source)
		if err != nil {
			t.Fatal(err)
		}
		if string(before) != string(after) || s.settings().Revision != revision {
			t.Fatal("rejected launch changed file or runtime")
		}
		h.Env = []string{"MODE="}
		set.Harnesses["custom"] = h
		if err := s.applySettings(set); err != nil {
			t.Fatal("explicit valid replacement must repair old environment", err)
		}
	}
}

func TestLaunchNodeSettingsPinnedIdentity(t *testing.T) {
	for _, edit := range []string{"command", "adapter"} {
		t.Run(edit, func(t *testing.T) {
			s := settingsFixture(t)
			set := s.settings()
			h := set.Harnesses["mock"]
			if edit == "command" {
				h.Command = "other-command"
			} else {
				empty := ""
				h.Adapter = &empty
			}
			set.Harnesses["mock"] = h
			if err := s.applySettings(set); err == nil {
				t.Fatal("pinned adapter identity changed online")
			}
		})
	}
}
