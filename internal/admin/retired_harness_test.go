package admin

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/configbuild"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/roster"
)

// An installation whose configuration still has a Kimi Code agent starts
// normally: the agent is listed as no longer supported, cannot start new
// work, and its owner can remove it.
func TestInstallationWithAKimiAgentStartsAndCanRemoveIt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	saved := &config.Config{
		Agents: map[string]config.Agent{
			"codex": {Harness: "codex", Aliases: []string{"codex"}, Default: true},
			"kimi":  {Harness: "kimi", Aliases: []string{"kimi", "kimi-code"}},
		},
		Harnesses: map[string]config.Harness{
			"codex": {Command: "/bin/sh", Permission: config.PermissionRead},
			"kimi":  {Command: "kimi", Args: []string{"acp"}, Permission: config.PermissionRead},
		},
		Projects: map[string]config.Project{"workspace": {Home: config.ProjectHome{Path: filepath.Join(dir, "work")}}},
		Gateway:  config.Gateway{StatePath: filepath.Join(dir, "state", "state.json")},
	}
	if err := config.Save(path, saved); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("configuration with a kimi agent does not load: %v", err)
	}
	catalog, err := cfg.AgentCatalog()
	if err != nil {
		t.Fatal(err)
	}
	manager, err := configbuild.HarnessManager(HarnessRuntimeConfig(cfg))
	if err != nil {
		t.Fatalf("harness manager refused a kimi harness: %v", err)
	}
	t.Cleanup(manager.Stop)

	fleet := roster.New(catalog)
	var kimi roster.Candidate
	for _, c := range fleet.All(t.Context()) {
		if c.Agent.ID == "kimi" {
			kimi = c
		}
	}
	if kimi.Agent.ID == "" || kimi.Eligible || kimi.Reason != roster.ReasonRetired || kimi.ReasonDetail != "Kimi Code" {
		t.Fatalf("kimi agent = %+v, want listed as no longer supported", kimi)
	}
	if _, err := manager.OpenSession(t.Context(), harness.Placement{Harness: "kimi"}, "", t.TempDir(), nil); !errors.Is(err, harness.ErrRetired) || !strings.Contains(err.Error(), "Kimi Code") {
		t.Fatalf("a kimi session was started: %v", err)
	}
	if _, err := manager.OpenSession(t.Context(), harness.Placement{Node: "elsewhere", Harness: "kimi"}, "", t.TempDir(), nil); !errors.Is(err, harness.ErrRetired) {
		t.Fatalf("a remote kimi session was started: %v", err)
	}

	a := &Service{NodeName: "node-hub", ConfigStore: NewConfigStore(cfg), Path: path, Catalog: catalog}
	if err := a.RemoveAgent(t.Context(), "kimi"); err != nil {
		t.Fatalf("owner cannot remove the kimi agent: %v", err)
	}
	if _, ok := a.Catalog.Resolve("kimi"); ok {
		t.Fatal("removed agent still published")
	}
	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded.Agents["kimi"]; ok {
		t.Fatal("removal was not saved")
	}
}
