package admin

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/configbuild"
	"github.com/gopact-ai/steve/internal/nodewire"
)

// A project homed on another machine used to stop a local agent: the turn
// was refused and the owner had to add a copy by hand. The project now
// follows the agent — the same relative directory, made on the machine
// that is about to work in it.
func TestEnsureProjectWorkspaceAttachesTheProjectWhereTheAgentRuns(t *testing.T) {
	a, _ := projectAdminFixture(t)
	item := a.Cfg.Projects["p"]
	item.Workspaces = nil
	a.Cfg.Projects["p"] = item
	if err := config.Save(a.Path, a.Cfg); err != nil {
		t.Fatal(err)
	}
	if err := (configbuild.ProjectController{Store: a.Projects}).Reconcile(t.Context(), a.Cfg); err != nil {
		t.Fatal(err)
	}
	if err := a.EnsureProjectWorkspace(t.Context(), "p", ""); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(nodewire.ProjectsDir(a.Cfg.LocalWorkspaceRoot()), "p")
	copies := a.Cfg.Projects["p"].Workspaces
	if len(copies) != 1 || copies[0].Node != "" || copies[0].Path != want {
		t.Fatalf("the project was not attached here: %+v, want %q", copies, want)
	}
	if info, err := os.Stat(want); err != nil || !info.IsDir() {
		t.Fatalf("the attached directory was not made: %v", err)
	}
	p, ok, err := a.Projects.Get(t.Context(), "p")
	if err != nil || !ok {
		t.Fatalf("project unreadable: %v", err)
	}
	if _, err := p.Place(""); err != nil {
		t.Fatalf("the attached copy cannot be worked in: %v", err)
	}
}

// Attaching what is already there changes nothing: a turn asks on every
// message, so the second answer must be the cheap one.
func TestEnsureProjectWorkspaceLeavesAPlacedProjectAlone(t *testing.T) {
	a, _ := projectAdminFixture(t)
	before := a.Cfg.Projects["p"].Workspaces
	if err := a.EnsureProjectWorkspace(t.Context(), "p", ""); err != nil {
		t.Fatal(err)
	}
	after := a.Cfg.Projects["p"].Workspaces
	if len(after) != len(before) || after[0].Path != before[0].Path {
		t.Fatalf("a placed project was changed: %+v -> %+v", before, after)
	}
}
