package node

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/gopact-ai/steve/internal/nodewire"
)

func settingsFixture(t *testing.T) *Server {
	t.Helper()
	cfg := ServerConfig{Source: filepath.Join(t.TempDir(), "node.json"), Name: "test", Harnesses: map[string]HarnessSpec{"mock": {Adapter: "codex-acp", Command: "resolved-adapter", Slots: 3, Env: []string{"SECRET=kept"}}}, MCPServers: map[string]MCPSpec{"remote": {Type: "http", URL: "https://example.test/mcp", Env: map[string]string{"SECRET": "kept"}, Headers: map[string]string{"Authorization": "kept"}}}}
	if err := writeConfig(cfg); err != nil {
		t.Fatal(err)
	}
	return NewServer(cfg)
}

func TestNodeSettingsCASRejectsStaleAndEmptyRevision(t *testing.T) {
	s := settingsFixture(t)
	before := s.settings()
	if before.Revision == "" {
		t.Fatal("GET omitted revision")
	}
	set := before
	set.Tools = []string{"git"}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Go(func() { results <- s.applySettings(set) })
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
	after := s.settings()
	if after.Revision == before.Revision {
		t.Fatal("successful change kept old revision")
	}
	after.Revision = ""
	if err := s.applySettings(after); !errors.Is(err, nodewire.ErrSettingsRevisionConflict) {
		t.Fatalf("missing revision accepted: %v", err)
	}
}

func TestNodeSettingsRoundTripPreservesHiddenFieldsAndExplicitClears(t *testing.T) {
	s := settingsFixture(t)
	original := s.conf()
	set := s.settings()
	set.Tools = []string{"git"}
	if err := s.applySettings(set); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original.Harnesses, s.conf().Harnesses) || !reflect.DeepEqual(original.MCPServers, s.conf().MCPServers) {
		t.Fatal("unrelated settings edit changed harness/secret fields")
	}
	set = s.settings()
	h := set.Harnesses["mock"]
	h.Adapter, h.Slots, h.Env = nil, nil, nil
	set.Harnesses["mock"] = h
	m := set.MCPServers["remote"]
	m.Env, m.Headers = nil, nil
	set.MCPServers["remote"] = m
	if err := s.applySettings(set); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original.Harnesses, s.conf().Harnesses) || !reflect.DeepEqual(original.MCPServers, s.conf().MCPServers) {
		t.Fatal("omitted fields did not preserve existing data")
	}
	set = s.settings()
	zero := 0
	h = set.Harnesses["mock"]
	h.Slots = &zero
	h.Env = []string{}
	set.Harnesses["mock"] = h
	m = set.MCPServers["remote"]
	m.Env = map[string]string{}
	m.Headers = map[string]string{}
	set.MCPServers["remote"] = m
	// Actual serialization must not collapse explicit empty values to omission.
	raw, err := json.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	var overWire nodewire.Settings
	if err := json.Unmarshal(raw, &overWire); err != nil {
		t.Fatal(err)
	}
	if err := s.applySettings(overWire); err != nil {
		t.Fatal(err)
	}
	after := s.conf()
	if after.Harnesses["mock"].Slots != 0 || len(after.Harnesses["mock"].Env) != 0 || len(after.MCPServers["remote"].Env) != 0 || len(after.MCPServers["remote"].Headers) != 0 {
		t.Fatal("explicit zero/empty lost over wire")
	}
	var disk ServerConfig
	bytes, err := os.ReadFile(after.Source)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(bytes, &disk); err != nil {
		t.Fatal(err)
	}
	if disk.Harnesses["mock"].Adapter != "codex-acp" || disk.Harnesses["mock"].Command != "" {
		t.Fatal("adapter declaration was replaced by generated command")
	}
}

func TestNodeSettingsDetectsExternalFileChanges(t *testing.T) {
	s := settingsFixture(t)
	set := s.settings()
	set.Tools = []string{"git"}
	raw, err := os.ReadFile(s.conf().Source)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(s.conf().Source, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.applySettings(set); !errors.Is(err, nodewire.ErrSettingsRevisionConflict) {
		t.Fatalf("external edit overwritten: %v", err)
	}
	after, _ := os.ReadFile(s.conf().Source)
	if string(after) != string(raw) {
		t.Fatal("external edit changed after conflict")
	}
}
