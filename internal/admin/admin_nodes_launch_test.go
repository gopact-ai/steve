package admin

import (
	"os"
	"reflect"
	"testing"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/nodewire"
)

func TestLaunchHubSettingsRejectsInvalidStartup(t *testing.T) {
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
			a := hubNodeSettingsFixture(t)
			before, err := os.ReadFile(a.Path)
			if err != nil {
				t.Fatal(err)
			}
			set := a.hubSettings()
			revision := set.Revision
			set.Harnesses["custom"] = nodewire.HarnessSetting{Command: tc.command, Args: tc.args, Env: tc.env}
			if _, err := a.SetNodeSettings(t.Context(), a.NodeName, set); err == nil {
				t.Fatal("invalid launch accepted")
			}
			after, err := os.ReadFile(a.Path)
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) || a.hubSettings().Revision != revision {
				t.Fatal("rejected launch changed file or runtime")
			}
		})
	}
}

func TestLaunchHubSettingsPinnedAdapterRejectsArgs(t *testing.T) {
	a := hubNodeSettingsFixture(t)
	before, err := os.ReadFile(a.Path)
	if err != nil {
		t.Fatal(err)
	}
	set := a.hubSettings()
	revision := set.Revision
	h := set.Harnesses["mock"]
	h.Args = []string{""}
	set.Harnesses["mock"] = h
	if _, err := a.SetNodeSettings(t.Context(), a.NodeName, set); err == nil {
		t.Fatal("pinned adapter accepted custom argv")
	}
	after, err := os.ReadFile(a.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) || a.hubSettings().Revision != revision {
		t.Fatal("rejected adapter edit changed file or runtime")
	}
}

func TestLaunchHubSettingsSaveReloadPreservesStartup(t *testing.T) {
	a := hubNodeSettingsFixture(t)
	a.cfg().Projects = map[string]config.Project{"work": {Home: config.ProjectHome{Path: t.TempDir()}}}
	want := config.Harness{Command: "未知 tool", Args: []string{"", "中文", "two words", `quoted"arg`, "$HOME", "--unknown"}, Env: []string{"MODE=", "_CUSTOM=中文 value=tail", "Path=one", "PATH=two"}, ProcessDir: t.TempDir(), Permission: config.PermissionRead}
	set := a.hubSettings()
	set.Harnesses["custom"] = nodewire.HarnessSetting{Command: want.Command, Args: want.Args, Env: want.Env, ProcessDir: want.ProcessDir}
	if _, err := a.SetNodeSettings(t.Context(), a.NodeName, set); err != nil {
		t.Fatal(err)
	}
	reloaded, err := config.Load(a.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reloaded.Harnesses["custom"], want) || !reflect.DeepEqual(a.cfg().Harnesses["custom"], want) {
		t.Fatal("saved startup differs from runtime/reloaded config")
	}
	pin := reloaded.Harnesses["mock"]
	if pin.Adapter != "codex-acp" || pin.Command != "" || len(pin.Args) != 0 {
		t.Fatal("adapter declaration did not reload")
	}
}

func TestLaunchHubSettingsValidatesRetainedEnv(t *testing.T) {
	for _, env := range [][]string{{"MODE=one", "MODE=two"}, {"STEVE_PROCESS_MARK=override"}} {
		a := hubNodeSettingsFixture(t)
		item := a.cfg().Harnesses["mock"]
		item.Env = env
		a.cfg().Harnesses["mock"] = item
		before, err := os.ReadFile(a.Path)
		if err != nil {
			t.Fatal(err)
		}
		set := a.hubSettings()
		revision := set.Revision
		h := set.Harnesses["mock"]
		h.Env = nil // Omission retains the old environment, so validate it too.
		set.Harnesses["mock"] = h
		if _, err := a.SetNodeSettings(t.Context(), a.NodeName, set); err == nil {
			t.Fatal("invalid retained adapter environment accepted")
		}
		after, err := os.ReadFile(a.Path)
		if err != nil {
			t.Fatal(err)
		}
		if string(before) != string(after) || a.hubSettings().Revision != revision {
			t.Fatal("rejected launch changed file or runtime")
		}
		h.Env = []string{"MODE="}
		set.Harnesses["mock"] = h
		if _, err := a.SetNodeSettings(t.Context(), a.NodeName, set); err != nil {
			t.Fatal("explicit valid replacement must repair old environment", err)
		}
	}
}

func TestLaunchHubSettingsPinnedIdentity(t *testing.T) {
	for _, edit := range []string{"command", "adapter"} {
		t.Run(edit, func(t *testing.T) {
			a := hubNodeSettingsFixture(t)
			set := a.hubSettings()
			h := set.Harnesses["mock"]
			if edit == "command" {
				h.Command = "other-command"
			} else {
				empty := ""
				h.Adapter = &empty
			}
			set.Harnesses["mock"] = h
			if _, err := a.SetNodeSettings(t.Context(), a.NodeName, set); err == nil {
				t.Fatal("pinned adapter identity changed online")
			}
		})
	}
}
