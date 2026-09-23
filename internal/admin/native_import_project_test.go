package admin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/configbuild"
	"github.com/gopact-ai/steve/internal/datalevel"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/project"
)

func nativeProjectFixture(t *testing.T) (*Service, config.Node, agent.Agent) {
	t.Helper()
	a, _ := projectAdminFixture(t)
	server := startAgentAdminNode(t, nil)
	target := config.Node{Addr: server.Addr(), Token: "test-node-token", Level: "restricted"}
	a.Cfg.Nodes["node-test"] = target
	a.Cfg.Agents["importer"] = config.Agent{Node: "node-test", Harness: "codex"}
	if err := config.Save(a.Path, a.Cfg); err != nil {
		t.Fatal(err)
	}
	a.Nodes = node.NewRegistry("hub-test", configbuild.NodeConfigs(a.Cfg))
	t.Cleanup(a.Nodes.Close)
	return a, target, agent.Agent{ID: "importer", Node: "node-test", Harness: "codex"}
}

func TestNativeImportAutoProjectConcurrentRetryUsesOneDeclaration(t *testing.T) {
	a, target, selected := nativeProjectFixture(t)
	dir := t.TempDir()
	const count = 5
	ids := make(chan string, count)
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for range count {
		wg.Go(func() {
			id, err := a.nativeImportProject(t.Context(), "node-test", target, selected, dir+"/.")
			ids <- id
			errs <- err
		})
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var want string
	for id := range ids {
		if want == "" {
			want = id
		}
		if id != want {
			t.Fatalf("concurrent imports chose %q and %q", want, id)
		}
	}
	p, ok, err := a.Projects.Get(t.Context(), want)
	if err != nil || !ok || p.Home.Path != dir || p.Home.Node != "node-test" || p.Repo != project.RepoInPlace || p.Level != datalevel.Internal {
		t.Fatalf("automatic project=%+v %v", p, err)
	}
	if len(a.Cfg.Projects) != 3 {
		t.Fatalf("duplicate declarations: %v", a.Cfg.Projects)
	}
	var stored config.Config
	raw, err := os.ReadFile(a.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Projects[want].Home.Path != dir {
		t.Fatal("automatic project was not durable")
	}
}

func TestNativeImportAutoProjectReusesExistingWorkspaceWithoutChangingPolicy(t *testing.T) {
	for _, copy := range []bool{false, true} {
		t.Run(map[bool]string{false: "home", true: "copy"}[copy], func(t *testing.T) {
			a, target, selected := nativeProjectFixture(t)
			dir := t.TempDir()
			p := config.Project{Home: config.ProjectHome{Node: "node-test", Path: dir}, Level: "restricted", Repo: "isolated", DefaultRole: "none"}
			if copy {
				p.Home = config.ProjectHome{Node: "remote", Path: "/existing-other"}
				p.Workspaces = []config.ProjectWorkspace{{Node: "node-test", Path: dir}}
			}
			if err := a.changeProjects(t.Context(), func(c *config.Config) error { c.Projects["existing"] = p; return nil }); err != nil {
				t.Fatal(err)
			}
			if copy {
				if _, err := a.Projects.SetCopy(t.Context(), "existing", project.Copy{Node: "node-test", Path: dir, Origin: project.OriginAdopted, State: project.CopyFailed}); err != nil {
					t.Fatal(err)
				}
			}
			writes := 0
			a.WriteConfig = func(string, *config.Config) error { writes++; return errors.New("read-only config") }
			id, err := a.nativeImportProject(t.Context(), "node-test", target, selected, dir)
			if err != nil || id != "existing" || writes != 0 {
				t.Fatalf("reuse=%q %v", id, err)
			}
			got, _, err := a.Projects.Get(t.Context(), id)
			if copy {
				if _, err := a.Projects.Materialize(t.Context(), project.Request{Project: id, Node: "node-test"}); err == nil {
					t.Fatal("failed workspace was made available by auto import")
				}
			}
			if err != nil || got.Level != datalevel.Restricted || got.Repo != project.RepoIsolated || got.DefaultRole != project.RoleNone || len(a.Cfg.Projects) != 3 {
				t.Fatalf("existing policy changed: %+v %v", got, err)
			}
		})
	}
}

func TestNativeImportAutoProjectFailuresDoNotRegisterDirectory(t *testing.T) {
	for _, kind := range []string{"missing", "relative", "node-replaced", "agent-replaced", "overlap", "save-failed", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			a, target, selected := nativeProjectFixture(t)
			dir := t.TempDir()
			ctx := t.Context()
			switch kind {
			case "missing":
				dir = filepath.Join(dir, "gone")
			case "relative":
				dir = "relative/path"
			case "node-replaced":
				n := a.Cfg.Nodes["node-test"]
				n.Token = "replaced"
				a.Cfg.Nodes["node-test"] = n
			case "agent-replaced":
				n := a.Cfg.Agents[selected.ID]
				n.Harness = "other"
				a.Cfg.Agents[selected.ID] = n
			case "overlap":
				if err := a.changeProjects(ctx, func(c *config.Config) error {
					c.Projects["parent"] = config.Project{Home: config.ProjectHome{Node: "node-test", Path: dir}}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				dir = filepath.Join(dir, "child")
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
			case "save-failed":
				a.WriteConfig = func(string, *config.Config) error { return errors.New("save failed") }
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			before := len(a.Cfg.Projects)
			if _, err := a.nativeImportProject(ctx, "node-test", target, selected, dir); err == nil {
				t.Fatal("invalid auto association succeeded")
			}
			if len(a.Cfg.Projects) != before {
				t.Fatal("failed automatic association left a project")
			}
		})
	}
}

func TestNativeImportAutoProjectRechecksIdentityAtCommit(t *testing.T) {
	a, target, selected := nativeProjectFixture(t)
	dir := t.TempDir()
	a.Mu.Lock()
	done := make(chan error, 1)
	go func() { _, err := a.nativeImportProject(t.Context(), "node-test", target, selected, dir); done <- err }()
	waitForAgentAdminCommit(t, "nativeImportProject")
	ConfigMu.Lock()
	replaced := a.Cfg.Nodes["node-test"]
	replaced.Token = "replacement"
	a.Cfg.Nodes["node-test"] = replaced
	ConfigMu.Unlock()
	a.Mu.Unlock()
	if err := <-done; err == nil {
		t.Fatal("stale inspected node was registered")
	}
	if len(a.Cfg.Projects) != 2 {
		t.Fatal("node replacement left a project")
	}
}

func TestNativeImportAutoProjectNamesDifferentDirectoriesWithoutCollisions(t *testing.T) {
	a, target, selected := nativeProjectFixture(t)
	var ids []string
	for range 2 {
		dir := filepath.Join(t.TempDir(), "my-project")
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		id, err := a.nativeImportProject(t.Context(), "node-test", target, selected, dir)
		if err != nil {
			t.Fatal(err)
		}
		again, err := a.nativeImportProject(t.Context(), "node-test", target, selected, dir)
		if err != nil || again != id {
			t.Fatalf("name unstable: %q -> %q %v", id, again, err)
		}
		ids = append(ids, id)
	}
	if ids[0] != "my-project" || ids[0] == ids[1] || !NameShape.MatchString(ids[1]) {
		t.Fatalf("automatic names=%v", ids)
	}
}
