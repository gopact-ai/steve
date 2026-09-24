package admin

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"sync"
	"testing"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/configbuild"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/roster"
)

func hubNodeSettingsFixture(t *testing.T) *Service {
	t.Helper()
	a := agentAdminFixture(t)
	a.cfg().Harnesses["mock"] = config.Harness{Command: "resolved-adapter", Adapter: "codex-acp", Slots: 3, Permission: config.PermissionRead, Env: []string{"SECRET=kept"}}
	a.cfg().MCPServers = map[string]config.MCPServer{"remote": {Type: "http", URL: "https://example.test/mcp", Env: map[string]string{"SECRET": "kept"}, Headers: map[string]string{"Authorization": "kept"}}}
	if err := config.Save(a.Path, a.cfg()); err != nil {
		t.Fatal(err)
	}
	manager, err := configbuild.HarnessManager(a.cfg())
	if err != nil {
		t.Fatal(err)
	}
	a.Manager = manager
	t.Cleanup(manager.Stop)
	a.Assembler = configbuild.CapabilityAssembler(a.cfg())
	a.Fleet = roster.New(a.Catalog)
	return a
}

func TestHubNodeSettingsCASAndLosslessRoundTrip(t *testing.T) {
	a := hubNodeSettingsFixture(t)
	original := a.cfg().Harnesses["mock"]
	before, err := a.NodeSettings(t.Context(), a.NodeName)
	if err != nil || before.Revision == "" {
		t.Fatalf("GET revision=%q err=%v", before.Revision, err)
	}
	set := before
	set.Tools = []string{"git"}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Go(func() { _, err := a.SetNodeSettings(t.Context(), a.NodeName, set); results <- err })
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
	if !reflect.DeepEqual(a.cfg().Harnesses["mock"], original) {
		t.Fatal("unrelated edit lost Adapter/Slots/Permission/Env")
	}
	set, _ = a.NodeSettings(t.Context(), a.NodeName)
	h := set.Harnesses["mock"]
	h.Adapter, h.Slots, h.Permission, h.Env = nil, nil, nil, nil
	set.Harnesses["mock"] = h
	m := set.MCPServers["remote"]
	m.Env, m.Headers = nil, nil
	set.MCPServers["remote"] = m
	if _, err := a.SetNodeSettings(t.Context(), a.NodeName, set); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a.cfg().Harnesses["mock"], original) || a.cfg().MCPServers["remote"].Env["SECRET"] != "kept" || a.cfg().MCPServers["remote"].Headers["Authorization"] != "kept" {
		t.Fatal("omitted fields cleared data")
	}
	set, _ = a.NodeSettings(t.Context(), a.NodeName)
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
	if _, err := a.SetNodeSettings(t.Context(), a.NodeName, posted); err != nil {
		t.Fatal(err)
	}
	got := a.cfg().Harnesses["mock"]
	if got.Slots != 0 || got.Adapter != original.Adapter || got.Permission != original.Permission || len(got.Env) != 0 || len(a.cfg().MCPServers["remote"].Env) != 0 || len(a.cfg().MCPServers["remote"].Headers) != 0 {
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
	if _, err := a.SetNodeSettings(t.Context(), a.NodeName, posted); !errors.Is(err, nodewire.ErrSettingsRevisionConflict) {
		t.Fatalf("empty revision accepted: %v", err)
	}
}

func TestHubNodeSettingsFileFailureDoesNotAdvanceRevision(t *testing.T) {
	a := hubNodeSettingsFixture(t)
	before, _ := a.NodeSettings(t.Context(), a.NodeName)
	set := before
	set.Tools = []string{"git"}
	a.WriteConfig = func(string, *config.Config) error { return errors.New("save unavailable") }
	if _, err := a.SetNodeSettings(t.Context(), a.NodeName, set); err == nil {
		t.Fatal("failed save reported success")
	}
	after, _ := a.NodeSettings(t.Context(), a.NodeName)
	if before.Revision != after.Revision {
		t.Fatal("failed save consumed base revision")
	}
}
