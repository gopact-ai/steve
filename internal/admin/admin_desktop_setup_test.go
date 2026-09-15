package admin

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/desktop"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

func TestDesktopStatusReportsTheWorkspaceAndWhereTheGuideStands(t *testing.T) {
	admin, _ := desktopAdminFixture(t)
	status, err := admin.DesktopStatus(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !status.Enabled || !status.SetupRequired || status.Setup == nil || status.Setup.Step != "identity" || status.Setup.Done {
		t.Fatalf("a fresh desktop opens the guide at its first page: %+v", status)
	}
	if status.WorkspacePath != admin.Cfg.Projects["workspace"].Home.Path || status.WorkspacePath == "" || !status.WorkspaceManaged {
		t.Fatalf("status should name the default project's directory and that it is still the managed one: %+v", status)
	}
	status, err = admin.DesktopSetup(t.Context(), consoleapi.DesktopSetupRequest{Step: "agents"})
	if err != nil || status.Setup.Step != "agents" || !status.SetupRequired {
		t.Fatalf("progress = %+v, %v", status, err)
	}
	if _, err := admin.DesktopSetup(t.Context(), consoleapi.DesktopSetupRequest{Step: "elsewhere"}); err == nil {
		t.Fatal("unknown steps are refused")
	}
	if _, err := admin.DesktopEnroll(t.Context(), consoleapi.DesktopEnrollRequest{AgentIDs: []string{"grok"}}); err != nil {
		t.Fatal(err)
	}
	status, _ = admin.DesktopStatus(t.Context())
	if !status.SetupRequired || status.AgentCount != 1 {
		t.Fatalf("registering an agent does not finish the guide: %+v", status)
	}
	status, err = admin.DesktopSetup(t.Context(), consoleapi.DesktopSetupRequest{Step: "finished", Done: true})
	if err != nil || status.SetupRequired || !status.Setup.Done {
		t.Fatalf("finishing = %+v, %v", status, err)
	}
	reopened := &Service{Cfg: admin.Cfg, Path: admin.Path}
	status, _ = reopened.DesktopStatus(t.Context())
	if status.SetupRequired || status.Setup == nil || !status.Setup.Done {
		t.Fatalf("progress survives a restart: %+v", status)
	}
}

func TestDesktopWorkspaceMovesTheDefaultProjectDirectory(t *testing.T) {
	admin, _ := desktopAdminFixture(t)
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	admin.Projects = project.Open(book)
	if err := (config.ProjectController{Store: admin.Projects}).Reconcile(t.Context(), admin.Cfg); err != nil {
		t.Fatal(err)
	}
	before := admin.Cfg.Projects["workspace"].Home.Path
	if _, err := admin.DesktopWorkspace(t.Context(), consoleapi.DesktopWorkspaceRequest{Path: "/etc"}); err == nil {
		t.Fatal("system directories are refused")
	}
	if admin.Cfg.Projects["workspace"].Home.Path != before {
		t.Fatal("a refused directory must not change the project")
	}
	status, err := admin.DesktopWorkspace(t.Context(), consoleapi.DesktopWorkspaceRequest{Path: "~/Steve"})
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, "Steve")
	if status.WorkspacePath != want {
		t.Fatalf("status reports the new directory: %+v", status)
	}
	if info, err := os.Stat(want); err != nil || !info.IsDir() {
		t.Fatalf("the directory is created: %v", err)
	}
	saved, err := config.Load(admin.Path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Projects["workspace"].Home.Path != want || saved.Gateway.DefaultProject != "workspace" {
		t.Fatalf("the configuration file records the move: %+v", saved.Projects["workspace"])
	}
	stored, ok, err := admin.Projects.Get(t.Context(), "workspace")
	if err != nil || !ok || stored.Home.Path != want {
		t.Fatalf("the project store follows: %+v %v %v", stored.Home, ok, err)
	}
	if status.WorkspaceManaged {
		t.Fatalf("a chosen directory is no longer the managed one: %+v", status)
	}
	again, err := admin.DesktopWorkspace(t.Context(), consoleapi.DesktopWorkspaceRequest{Path: want})
	if err != nil || again.WorkspacePath != want {
		t.Fatalf("choosing the same directory again is fine: %+v %v", again, err)
	}
	if _, err := admin.DesktopWorkspace(t.Context(), consoleapi.DesktopWorkspaceRequest{Path: filepath.Dir(admin.Path)}); err == nil {
		t.Fatal("the desktop's own state directory is refused")
	}
}

func TestDesktopWorkspaceLeavesAProjectOnAnotherMachineAlone(t *testing.T) {
	admin, _ := desktopAdminFixture(t)
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	admin.Projects = project.Open(book)
	if err := (config.ProjectController{Store: admin.Projects}).Reconcile(t.Context(), admin.Cfg); err != nil {
		t.Fatal(err)
	}
	item := admin.Cfg.Projects["workspace"]
	item.Home = config.ProjectHome{Node: "gpu-box", Path: "/srv/steve"}
	admin.Cfg.Projects["workspace"] = item
	_, err = admin.DesktopWorkspace(t.Context(), consoleapi.DesktopWorkspaceRequest{Path: "~/Elsewhere"})
	if err == nil || !desktop.IsInputError(err) {
		t.Fatalf("a remote default project is refused as the owner's request: %v", err)
	}
	home, _ := os.UserHomeDir()
	if _, err := os.Stat(filepath.Join(home, "Elsewhere")); err == nil {
		t.Fatal("nothing is created for a refused move")
	}
	if err := admin.SetProjectHome(t.Context(), "workspace", "/tmp/steve"); err == nil {
		t.Fatal("SetProjectHome itself refuses a remote project")
	}
	if admin.Cfg.Projects["workspace"].Home.Path != "/srv/steve" {
		t.Fatalf("the remote project is untouched: %+v", admin.Cfg.Projects["workspace"].Home)
	}
}
