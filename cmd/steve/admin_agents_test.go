package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
)

func TestFirstAgentRegistrationBecomesTheDefault(t *testing.T) {
	admin := agentAdminFixture(t)
	admin.cfg.Agents = nil
	catalog, err := admin.cfg.AgentCatalog()
	if err != nil {
		t.Fatal(err)
	}
	admin.catalog.Publish(catalog)
	if err := admin.AddAgent(t.Context(), consoleapi.AddAgentRequest{ID: "first", Harness: "mock"}); err != nil {
		t.Fatal(err)
	}
	if !admin.cfg.Agents["first"].Default || admin.catalog.Default().ID != "first" {
		t.Fatal("first registered Agent is not the default")
	}
	if err := admin.AddAgent(t.Context(), consoleapi.AddAgentRequest{ID: "second", Harness: "mock"}); err != nil {
		t.Fatal(err)
	}
	if admin.cfg.Agents["second"].Default || admin.catalog.Default().ID != "first" {
		t.Fatal("later registration changed the default")
	}
}

func agentAdminFixture(t *testing.T) *fleetAdmin {
	t.Helper()
	cfg := &config.Config{
		Harnesses: map[string]config.Harness{"mock": {Command: "mock"}},
		Agents: map[string]config.Agent{
			"primary": {Harness: "mock", Default: true, Aliases: []string{"main"}},
			"worker":  {Harness: "mock", Model: "before", Options: map[string]string{"effort": "high"}, Skills: []string{"skill"}},
		},
	}
	catalog, err := cfg.AgentCatalog()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	return &fleetAdmin{cfg: cfg, path: path, catalog: catalog}
}

func TestAgentChangesPublishAnAlreadyCommittedConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*fleetAdmin) error
	}{
		{"add", func(a *fleetAdmin) error {
			return a.AddAgent(t.Context(), consoleapi.AddAgentRequest{ID: "new", Harness: "mock"})
		}},
		{"update", func(a *fleetAdmin) error {
			return a.UpdateAgent(t.Context(), "worker", consoleapi.AgentSpec{Harness: "mock", Model: "after"})
		}},
		{"remove", func(a *fleetAdmin) error { return a.RemoveAgent(t.Context(), "worker") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			admin := agentAdminFixture(t)
			before := admin.catalog.List()
			beforeConfig, _ := json.Marshal(admin.cfg)
			admin.writeConfig = func(path string, candidate *config.Config) error {
				currentConfig, _ := json.Marshal(admin.cfg)
				if !reflect.DeepEqual(before, admin.catalog.List()) || string(currentConfig) != string(beforeConfig) {
					t.Fatal("candidate was published before persistence completed")
				}
				if err := config.Save(path, candidate); err != nil {
					return err
				}
				return &config.CommittedError{Err: errors.New("directory sync failed")}
			}
			err := tc.change(admin)
			if !config.Committed(err) || !strings.Contains(err.Error(), "配置已应用，目录同步失败") {
				t.Fatalf("commit status was lost: %v", err)
			}
			raw, err := os.ReadFile(admin.path)
			if err != nil {
				t.Fatal(err)
			}
			var stored config.Config
			if err := json.Unmarshal(raw, &stored); err != nil {
				t.Fatal(err)
			}
			restored, err := stored.AgentCatalog()
			if err != nil {
				t.Fatal(err)
			}
			if reflect.DeepEqual(before, admin.catalog.List()) || !reflect.DeepEqual(restored.List(), admin.catalog.List()) || !reflect.DeepEqual(stored.Agents, admin.cfg.Agents) {
				t.Fatal("post-rename failure incorrectly rolled back the committed candidate")
			}
		})
	}
}

func TestAgentChangesLeaveAllStateUntouchedWhenPersistenceFails(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*fleetAdmin) error
	}{
		{"add", func(a *fleetAdmin) error {
			return a.AddAgent(t.Context(), consoleapi.AddAgentRequest{ID: "new", Harness: "mock"})
		}},
		{"update", func(a *fleetAdmin) error {
			return a.UpdateAgent(t.Context(), "worker", consoleapi.AgentSpec{Harness: "mock", Model: "after", Options: map[string]string{"effort": "low"}})
		}},
		{"remove", func(a *fleetAdmin) error { return a.RemoveAgent(t.Context(), "worker") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			admin := agentAdminFixture(t)
			before, _ := json.Marshal(admin.cfg)
			catalog := admin.catalog.List()
			originalPath := admin.path
			disk, err := os.ReadFile(originalPath)
			if err != nil {
				t.Fatal(err)
			}
			// An existing regular file cannot be the config's parent directory.
			admin.path = filepath.Join(originalPath, "unwritable.json")
			if err := tc.change(admin); err == nil {
				t.Fatal("persistence failure was reported as success")
			}
			after, _ := json.Marshal(admin.cfg)
			if string(before) != string(after) {
				t.Fatal("rejected change mutated the live configuration")
			}
			if !reflect.DeepEqual(catalog, admin.catalog.List()) {
				t.Fatal("rejected change published a different catalog")
			}
			stored, err := os.ReadFile(originalPath)
			if err != nil || string(stored) != string(disk) {
				t.Fatalf("rejected change altered persistent configuration: %v", err)
			}
		})
	}
}

func TestAgentChangesPersistTheSameCandidateTheyPublish(t *testing.T) {
	admin := agentAdminFixture(t)
	if err := admin.AddAgent(t.Context(), consoleapi.AddAgentRequest{ID: "new", Harness: "mock", Model: "first"}); err != nil {
		t.Fatal(err)
	}
	if err := admin.UpdateAgent(t.Context(), "worker", consoleapi.AgentSpec{Harness: "mock", Model: "after", Options: map[string]string{"effort": "low"}}); err != nil {
		t.Fatal(err)
	}
	if err := admin.RemoveAgent(t.Context(), "new"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(admin.path)
	if err != nil {
		t.Fatal(err)
	}
	var stored config.Config
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	restored, err := stored.AgentCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stored.Agents, admin.cfg.Agents) || !reflect.DeepEqual(restored.List(), admin.catalog.List()) {
		t.Fatal("live config, published catalog and restart config disagree")
	}
	worker, _ := admin.catalog.Resolve("worker")
	if worker.Model != "after" || !reflect.DeepEqual(worker.Skills, []string{"skill"}) {
		t.Fatal("update did not preserve the unedited configuration")
	}
}

func TestInvalidAgentCandidateDoesNotReachPersistence(t *testing.T) {
	admin := agentAdminFixture(t)
	admin.writeConfig = func(string, *config.Config) error {
		t.Fatal("invalid candidate reached persistence")
		return nil
	}
	before, err := os.ReadFile(admin.path)
	if err != nil {
		t.Fatal(err)
	}
	// "main" is an alias owned by the primary agent.
	if err := admin.AddAgent(t.Context(), consoleapi.AddAgentRequest{ID: "main", Harness: "mock"}); err == nil {
		t.Fatal("duplicate alias accepted")
	}
	if err := admin.RemoveAgent(t.Context(), "primary"); err == nil {
		t.Fatal("default agent removed")
	}
	after, err := os.ReadFile(admin.path)
	if err != nil || string(before) != string(after) {
		t.Fatalf("invalid candidate reached persistence: %v", err)
	}
}
