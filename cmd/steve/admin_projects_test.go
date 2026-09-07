package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

func projectAdminFixture(t *testing.T) (*fleetAdmin, *ledger.Ledger) {
	t.Helper()
	admin := agentAdminFixture(t)
	admin.cfg.Nodes = map[string]config.Node{"remote": {Addr: "127.0.0.1:1", Token: "test"}}
	admin.cfg.Gateway.HomePath = filepath.Join(t.TempDir(), "home")
	admin.cfg.Gateway.DefaultProject = "p"
	admin.cfg.Projects = map[string]config.Project{
		"p":      {Home: config.ProjectHome{Node: "remote", Path: "/remote-project"}, Workspaces: []config.ProjectWorkspace{{Path: t.TempDir()}}},
		"remove": {Home: config.ProjectHome{Path: t.TempDir()}},
	}
	if err := config.Save(admin.path, admin.cfg); err != nil {
		t.Fatal(err)
	}
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	admin.projects = project.Open(book)
	if err := (config.ProjectController{Store: admin.projects}).Reconcile(t.Context(), admin.cfg); err != nil {
		t.Fatal(err)
	}
	return admin, book
}

func TestProjectManagementFileFailureLeavesCandidateUnpublished(t *testing.T) {
	for _, name := range []string{"add-project", "remove-project", "add-workspace", "remove-workspace"} {
		t.Run(name, func(t *testing.T) {
			a, _ := projectAdminFixture(t)
			if name == "add-workspace" {
				p := a.cfg.Projects["p"]
				p.Workspaces = nil
				a.cfg.Projects["p"] = p
				if err := config.Save(a.path, a.cfg); err != nil {
					t.Fatal(err)
				}
				if err := (config.ProjectController{Store: a.projects}).Reconcile(t.Context(), a.cfg); err != nil {
					t.Fatal(err)
				}
			}
			raw, err := os.ReadFile(a.path)
			if err != nil {
				t.Fatal(err)
			}
			beforeConfig, _ := json.Marshal(a.cfg)
			beforeProjects, err := a.projects.List(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			cause := errors.New("file unavailable")
			a.writeConfig = func(string, *config.Config) error { return cause }
			switch name {
			case "add-project":
				err = a.AddProject(t.Context(), consoleapi.AddProjectRequest{ID: "new", Path: t.TempDir()})
			case "remove-project":
				err = a.RemoveProject(t.Context(), "remove")
			case "add-workspace":
				err = a.AddWorkspace(t.Context(), "p", consoleapi.AddWorkspaceRequest{Path: t.TempDir()})
			case "remove-workspace":
				err = a.RemoveWorkspace(t.Context(), "p", "")
			}
			if !errors.Is(err, cause) {
				t.Fatalf("file failure was not reached or reported: %v", err)
			}
			afterConfig, _ := json.Marshal(a.cfg)
			if string(beforeConfig) != string(afterConfig) {
				t.Fatal("failed operation mutated live configuration")
			}
			afterProjects, err := a.projects.List(t.Context())
			if err != nil || !reflect.DeepEqual(beforeProjects, afterProjects) {
				t.Fatalf("failed operation mutated or blocked prior projection: %v", err)
			}
			saved, err := os.ReadFile(a.path)
			if err != nil || string(saved) != string(raw) {
				t.Fatal("failed operation modified persistent config")
			}
		})
	}
}

func TestProjectManagementRetryReconcilesCommittedCandidate(t *testing.T) {
	a, book := projectAdminFixture(t)
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
		_, err := tx.Exec(`CREATE TRIGGER stop_reconcile BEFORE INSERT ON bindings WHEN NEW.kind = 'project-declarations' BEGIN SELECT RAISE(ABORT, 'projection unavailable'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	req := consoleapi.AddProjectRequest{ID: "new", Path: t.TempDir()}
	var pending *config.ProjectionPendingError
	if err := a.AddProject(t.Context(), req); !errors.As(err, &pending) {
		t.Fatalf("committed failure missing pending receipt: %v", err)
	}
	if _, ok := a.cfg.Projects["new"]; !ok {
		t.Fatal("committed candidate rolled back")
	}
	if _, _, err := a.projects.Get(t.Context(), "p"); !errors.Is(err, project.ErrDeclarationPending) {
		t.Fatalf("stale projection remained live: %v", err)
	}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error { _, err := tx.Exec("DROP TRIGGER stop_reconcile"); return err }); err != nil {
		t.Fatal(err)
	}
	if err := a.AddProject(t.Context(), req); err != nil {
		t.Fatalf("same request could not recover committed declaration: %v", err)
	}
	if _, ok, err := a.projects.Get(t.Context(), "new"); err != nil || !ok {
		t.Fatalf("retry did not reconcile new project: %v", err)
	}
}
