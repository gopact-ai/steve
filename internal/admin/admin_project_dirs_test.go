package admin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
)

// A project is a directory under the machine's workspace, so the same
// project is the same relative directory everywhere. Anything else drifts:
// a copy on a second machine would land where the first has never looked.
func TestAddProjectNamesADirectoryUnderThisMachineWorkspace(t *testing.T) {
	a, _ := projectAdminFixture(t)
	if err := a.AddProject(t.Context(), consoleapi.AddProjectRequest{ID: "new", Path: "new-service"}); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(nodewire.ProjectsDir(a.Cfg.LocalWorkspaceRoot()), "new-service")
	if got := a.Cfg.Projects["new"].Home.Path; got != want {
		t.Fatalf("project directory %q is not under the workspace %q", got, want)
	}
	if info, err := os.Stat(want); err != nil || !info.IsDir() {
		t.Fatalf("the directory was not made for the owner: %v", err)
	}
}

// Naming nothing is naming the project: the common case types once.
func TestAddProjectWithoutADirectoryUsesTheProjectName(t *testing.T) {
	a, _ := projectAdminFixture(t)
	if err := a.AddProject(t.Context(), consoleapi.AddProjectRequest{ID: "new"}); err != nil {
		t.Fatal(err)
	}
	if got, want := a.Cfg.Projects["new"].Home.Path, filepath.Join(nodewire.ProjectsDir(a.Cfg.LocalWorkspaceRoot()), "new"); got != want {
		t.Fatalf("project directory %q is not %q", got, want)
	}
}

func TestAddProjectRefusesADirectoryOutsideTheWorkspace(t *testing.T) {
	for _, dir := range []string{"/tmp/steve-project", "~/steve-project", "../escape", "./here", ""} {
		a, _ := projectAdminFixture(t)
		err := a.AddProject(t.Context(), consoleapi.AddProjectRequest{ID: "new", Path: dir})
		if dir == "" {
			continue
		}
		if err == nil {
			t.Fatalf("directory %q was accepted; it is not under the workspace", dir)
		}
		if _, declared := a.Cfg.Projects["new"]; declared {
			t.Fatalf("directory %q was refused but the project was declared", dir)
		}
	}
}

// Adopting used to refuse a directory that was not there, which left the
// owner to go and make it by hand on the right machine. The machine makes
// it now: naming a project is asking for it.
func TestAddWorkspaceMakesTheDirectoryItAdopts(t *testing.T) {
	a, _ := projectAdminFixture(t)
	item := a.Cfg.Projects["p"]
	item.Workspaces = nil
	a.Cfg.Projects["p"] = item
	if err := config.Save(a.Path, a.Cfg); err != nil {
		t.Fatal(err)
	}
	if err := (config.ProjectController{Store: a.Projects}).Reconcile(t.Context(), a.Cfg); err != nil {
		t.Fatal(err)
	}
	if err := a.AddWorkspace(t.Context(), "p", consoleapi.AddWorkspaceRequest{Path: "p"}); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(nodewire.ProjectsDir(a.Cfg.LocalWorkspaceRoot()), "p")
	copies := a.Cfg.Projects["p"].Workspaces
	if len(copies) != 1 || copies[0].Path != want {
		t.Fatalf("copy landed outside the workspace: %+v", copies)
	}
	if info, err := os.Stat(want); err != nil || !info.IsDir() {
		t.Fatalf("the adopted directory was not made: %v", err)
	}
}

// Another machine's workspace is what that machine says it is, so a
// project's directory is resolved where it will actually be used.
func TestAddProjectResolvesAnotherMachineWorkspaceFromItsAdvert(t *testing.T) {
	server := startAgentAdminNode(t, map[string]node.HarnessSpec{})
	a, _ := projectAdminFixture(t)
	a.Cfg.Nodes["node-test"] = config.Node{Addr: server.Addr(), Token: "test-node-token"}
	if err := config.Save(a.Path, a.Cfg); err != nil {
		t.Fatal(err)
	}
	a.Nodes = node.NewRegistry("hub-test", a.Cfg.NodeConfigs())
	t.Cleanup(a.Nodes.Close)
	advert, err := a.Nodes.Advert(t.Context(), "node-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.AddProject(t.Context(), consoleapi.AddProjectRequest{ID: "new", Node: "node-test", Path: "new-service"}); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(nodewire.ProjectsDir(advert.WorkspaceRoot), "new-service")
	if got := a.Cfg.Projects["new"].Home; got.Node != "node-test" || got.Path != want {
		t.Fatalf("project on another machine landed at %+v, want %q", got, want)
	}
	if info, err := os.Stat(want); err != nil || !info.IsDir() {
		t.Fatalf("the directory was not made on that machine: %v", err)
	}
}

func TestCheckProjectDirKeepsTheNameItAccepts(t *testing.T) {
	for _, dir := range []string{"my-service", "team/my-service", "/my-service"} {
		got, err := CheckProjectDir(dir)
		if strings.HasPrefix(dir, "/") {
			if err == nil {
				t.Fatalf("%q was accepted as a relative directory", dir)
			}
			continue
		}
		if err != nil || got != dir {
			t.Fatalf("CheckProjectDir(%q) = %q, %v", dir, got, err)
		}
	}
}
