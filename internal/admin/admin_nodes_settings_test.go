package admin

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"sync"
	"testing"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/roster"
)

func hubNodeSettingsFixture(t *testing.T) *Service {
	t.Helper()
	a := agentAdminFixture(t)
	a.Cfg.Harnesses["mock"] = config.Harness{Command: "resolved-adapter", Adapter: "codex-acp", Slots: 3, Permission: config.PermissionRead, Env: []string{"SECRET=kept"}}
	a.Cfg.MCPServers = map[string]config.MCPServer{"remote": {Type: "http", URL: "https://example.test/mcp", Env: map[string]string{"SECRET": "kept"}, Headers: map[string]string{"Authorization": "kept"}}}
	if err := config.Save(a.Path, a.Cfg); err != nil {
		t.Fatal(err)
	}
	manager, err := a.Cfg.HarnessManager()
	if err != nil {
		t.Fatal(err)
	}
	a.Manager = manager
	t.Cleanup(manager.Stop)
	a.Assembler = a.Cfg.CapabilityAssembler()
	a.Fleet = roster.New(a.Catalog)
	return a
}

func TestHubNodeSettingsCASAndLosslessRoundTrip(t *testing.T) {
	a := hubNodeSettingsFixture(t)
	original := a.Cfg.Harnesses["mock"]
	before, err := a.NodeSettings(t.Context(), NodeName())
	if err != nil || before.Revision == "" {
		t.Fatalf("GET revision=%q err=%v", before.Revision, err)
	}
	set := before
	set.Tools = []string{"git"}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Go(func() { _, err := a.SetNodeSettings(t.Context(), NodeName(), set); results <- err })
	}
	wg.Wait()
	close(results)
	wins, conflicts := 0, 0
	for err := range results {
		if err == nil {
			wins++
		} else if errors.Is(err, nodewire.ErrSettingsRevisionConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("CAS winners=%d conflicts=%d", wins, conflicts)
	}
	if !reflect.DeepEqual(a.Cfg.Harnesses["mock"], original) {
		t.Fatal("unrelated edit lost Adapter/Slots/Permission/Env")
	}
	set, _ = a.NodeSettings(t.Context(), NodeName())
	h := set.Harnesses["mock"]
	h.Adapter, h.Slots, h.Permission, h.Env = nil, nil, nil, nil
	set.Harnesses["mock"] = h
	m := set.MCPServers["remote"]
	m.Env, m.Headers = nil, nil
	set.MCPServers["remote"] = m
	if _, err := a.SetNodeSettings(t.Context(), NodeName(), set); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a.Cfg.Harnesses["mock"], original) || a.Cfg.MCPServers["remote"].Env["SECRET"] != "kept" || a.Cfg.MCPServers["remote"].Headers["Authorization"] != "kept" {
		t.Fatal("omitted fields cleared data")
	}
	set, _ = a.NodeSettings(t.Context(), NodeName())
	h = set.Harnesses["mock"]
	zero := 0
	h.Slots = &zero
	h.Env = []string{}
	set.Harnesses["mock"] = h
	m = set.MCPServers["remote"]
	m.Env = map[string]string{}
	m.Headers = map[string]string{}
	set.MCPServers["remote"] = m
	raw, err := json.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	var posted nodewire.Settings
	if err := json.Unmarshal(raw, &posted); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetNodeSettings(t.Context(), NodeName(), posted); err != nil {
		t.Fatal(err)
	}
	got := a.Cfg.Harnesses["mock"]
	if got.Slots != 0 || got.Adapter != original.Adapter || got.Permission != original.Permission || len(got.Env) != 0 || len(a.Cfg.MCPServers["remote"].Env) != 0 || len(a.Cfg.MCPServers["remote"].Headers) != 0 {
		t.Fatal("explicit clears were not preserved")
	}
	var disk config.Config
	raw, err = os.ReadFile(a.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &disk); err != nil {
		t.Fatal(err)
	}
	if disk.Harnesses["mock"].Command != "" || disk.Harnesses["mock"].Adapter != original.Adapter {
		t.Fatal("persisted adapter pin changed")
	}
	posted.Revision = ""
	if _, err := a.SetNodeSettings(t.Context(), NodeName(), posted); !errors.Is(err, nodewire.ErrSettingsRevisionConflict) {
		t.Fatalf("empty revision accepted: %v", err)
	}
}

func TestHubNodeSettingsFileFailureDoesNotAdvanceRevision(t *testing.T) {
	a := hubNodeSettingsFixture(t)
	before, _ := a.NodeSettings(t.Context(), NodeName())
	set := before
	set.Tools = []string{"git"}
	a.WriteConfig = func(string, *config.Config) error { return errors.New("save unavailable") }
	if _, err := a.SetNodeSettings(t.Context(), NodeName(), set); err == nil {
		t.Fatal("failed save reported success")
	}
	after, _ := a.NodeSettings(t.Context(), NodeName())
	if before.Revision != after.Revision {
		t.Fatal("failed save consumed base revision")
	}
}
