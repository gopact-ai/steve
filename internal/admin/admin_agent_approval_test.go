package admin

import (
	"path/filepath"
	"testing"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
)

func TestAgentApprovalOverrideAndResetPreserveGlobalDefault(t *testing.T) {
	admin := approvalAdminFixture(t, "ask")
	for _, mode := range []string{"agent-full-access", ""} {
		options := map[string]string{"effort": "high"}
		if mode != "" {
			options["mode"] = mode
		}
		if err := admin.UpdateAgent(t.Context(), "pinned", consoleapi.AgentSpec{Harness: "mock", Options: options}); err != nil {
			t.Fatal(err)
		}
		selected := admin.Catalog.Default()
		if selected.Approval != "ask" || admin.Cfg.Gateway.DefaultApproval != "ask" {
			t.Fatal("an Agent override changed the global default")
		}
		if selected.Options["mode"] != mode || selected.Options["effort"] != "high" {
			t.Fatalf("Agent approval was not published independently: %v", selected.Options)
		}
	}
}

// Syncing the fleet lets every agent go of the approval mode it pinned for
// itself, so the hub's default is the one place the stance is decided; the
// agents' other pins are none of its business.
func TestSyncAgentApprovalClearsPinnedModesOnly(t *testing.T) {
	admin := approvalAdminFixture(t, "auto")
	out, err := admin.SyncAgentApproval(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if out.Intent != "auto" || len(out.Cleared) != 2 || out.Cleared[0].Agent != "pinned" || out.Cleared[0].Was != "read-only" {
		t.Fatalf("sync reported %+v", out)
	}
	if len(out.Following) != 1 || out.Following[0] != "free" {
		t.Fatalf("agents already following the default were reported as %v", out.Following)
	}
	if _, pinned := admin.Cfg.Agents["pinned"].Options["mode"]; pinned {
		t.Fatal("an agent kept its own approval mode")
	}
	if admin.Cfg.Agents["pinned"].Options["effort"] != "high" {
		t.Fatal("syncing approval touched another selector")
	}
	if admin.Cfg.Agents["renamed"].Options != nil {
		t.Fatalf("a mode under the tool's own selector id survived: %v", admin.Cfg.Agents["renamed"].Options)
	}
	if got := admin.Catalog.Default().Options["mode"]; got != "" {
		t.Fatalf("the running catalog still pins %q", got)
	}
}

// Nothing to follow is an error the owner can act on rather than a silent
// pass that leaves every agent where it was.
func TestSyncAgentApprovalNeedsADefault(t *testing.T) {
	admin := approvalAdminFixture(t, "")
	if _, err := admin.SyncAgentApproval(t.Context()); err == nil {
		t.Fatal("syncing to no default was accepted")
	}
	if admin.Cfg.Agents["pinned"].Options["mode"] != "read-only" {
		t.Fatal("a refused sync changed an agent anyway")
	}
}

func approvalAdminFixture(t *testing.T, intent string) *Service {
	t.Helper()
	cfg := &config.Config{
		Gateway:   config.Gateway{DefaultApproval: intent},
		Harnesses: map[string]config.Harness{"mock": {Command: "mock"}},
		Agents: map[string]config.Agent{
			"pinned":  {Harness: "mock", Default: true, Options: map[string]string{"mode": "read-only", "effort": "high"}},
			"renamed": {Harness: "mock", Options: map[string]string{"mode": "plan"}},
			"free":    {Harness: "mock", Options: map[string]string{"effort": "low"}},
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
	return &Service{Cfg: cfg, Path: path, Catalog: catalog}
}
