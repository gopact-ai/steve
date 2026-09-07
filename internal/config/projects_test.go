package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

func controllerFixture(t *testing.T) (*Config, string, ProjectController, *ledger.Ledger) {
	t.Helper()
	cfg := &Config{Agents: map[string]Agent{"main": {Harness: "mock", Default: true}}, Harnesses: map[string]Harness{"mock": {Command: "mock"}}, Projects: map[string]Project{"p": {Home: ProjectHome{Path: "/project"}}}, Gateway: Gateway{HomePath: "/steve-home", DefaultProject: "p"}}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	c := ProjectController{Store: project.Open(book)}
	if err := c.Reconcile(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	return cfg, path, c, book
}

func TestProjectCandidateDoesNotPublishBeforeFileCommit(t *testing.T) {
	cfg, path, controller, _ := controllerFixture(t)
	candidate := CloneProjects(cfg)
	candidate.Projects["bad"] = Project{Home: ProjectHome{Path: "/project/nested"}}
	writes := 0
	if err := controller.Commit(t.Context(), candidate, func() error { writes++; return nil }); err == nil {
		t.Fatal("invalid ownership accepted")
	}
	if writes != 0 {
		t.Fatal("invalid candidate reached file commit")
	}
	delete(candidate.Projects, "bad")
	candidate.Projects["new"] = Project{Home: ProjectHome{Path: "/new"}}
	cause := errors.New("file write failed")
	if err := controller.Commit(t.Context(), candidate, func() error { return cause }); !errors.Is(err, cause) {
		t.Fatalf("write failure lost: %v", err)
	}
	if _, ok, _ := controller.Store.Get(t.Context(), "new"); ok {
		t.Fatal("failed file write published projection")
	}
	stored, err := Load(path)
	if err != nil || len(stored.Projects) != 1 {
		t.Fatalf("failed commit changed file: %v", err)
	}
}

func TestCommittedProjectFailureRecoversFromFileAfterRestart(t *testing.T) {
	cfg, path, controller, book := controllerFixture(t)
	candidate := CloneProjects(cfg)
	delete(candidate.Projects, "p")
	candidate.Projects["new"] = Project{Home: ProjectHome{Path: "/new"}}
	candidate.Gateway.DefaultProject = "new"
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
		_, err := tx.Exec(`CREATE TRIGGER block_project_reconcile BEFORE INSERT ON bindings WHEN NEW.kind = 'project-declarations' BEGIN SELECT RAISE(ABORT, 'injected reconcile failure'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	err := controller.Commit(t.Context(), candidate, func() error {
		err := Save(path, candidate)
		if err == nil || Committed(err) {
			*cfg = *candidate
		}
		return err
	})
	var pending *ProjectionPendingError
	if !errors.As(err, &pending) || pending.Hash == "" {
		t.Fatalf("post-commit failure lost pending status: %v", err)
	}
	if _, _, err := controller.Store.Get(t.Context(), "p"); !errors.Is(err, project.ErrDeclarationPending) {
		t.Fatalf("old projection remained usable: %v", err)
	}
	loaded, err := Load(path)
	if err != nil || loaded.Gateway.DefaultProject != "new" {
		t.Fatalf("committed configuration rolled back: %v", err)
	}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error { _, err := tx.Exec("DROP TRIGGER block_project_reconcile"); return err }); err != nil {
		t.Fatal(err)
	}
	restarted := ProjectController{Store: project.Open(book)}
	if err := restarted.Reconcile(t.Context(), loaded); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := restarted.Store.Get(t.Context(), "new"); err != nil || !ok {
		t.Fatalf("restart failed to apply desired addition: %v", err)
	}
	if _, ok, err := restarted.Store.Get(t.Context(), "p"); err != nil || ok {
		t.Fatalf("restart resurrected removed project: %v", err)
	}
	if _, ok, err := restarted.Store.GetHistorical(t.Context(), "p"); err != nil || !ok {
		t.Fatalf("retired metadata lost: %v", err)
	}
	if _, ok, _ := restarted.Store.Get(t.Context(), ReservedHomeProject); !ok {
		t.Fatal("complete reconciliation lost reserved home")
	}
}

func TestProjectCommitReconcilesAfterDirectorySyncWarning(t *testing.T) {
	cfg, path, controller, _ := controllerFixture(t)
	candidate := CloneProjects(cfg)
	candidate.Projects["new"] = Project{Home: ProjectHome{Path: "/new"}}
	err := controller.Commit(t.Context(), candidate, func() error {
		err := saveWithSync(path, candidate, func(string) error { return errors.New("cannot sync parent") })
		if err == nil || Committed(err) {
			*cfg = *candidate
		}
		return err
	})
	if !Committed(err) {
		t.Fatalf("directory warning lost: %v", err)
	}
	if _, ok, err := controller.Store.Get(t.Context(), "new"); err != nil || !ok {
		t.Fatalf("visible file was not projected: %v", err)
	}
	if err := Save(path, cfg); err != nil {
		t.Fatalf("committed revision was not adopted: %v", err)
	}
}

func TestConfigRejectsManualEditFromLoadAndKeepsAdapterDeclarative(t *testing.T) {
	cfg, path, _, _ := controllerFixture(t)
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	manual := CloneProjects(cfg)
	manual.Projects["manual"] = Project{Home: ProjectHome{Path: "/manual"}}
	if err := Save(path, manual); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	loaded.Projects["automatic"] = Project{Home: ProjectHome{Path: "/automatic"}}
	if err := Save(path, loaded); !errors.Is(err, ErrFileChanged) {
		t.Fatalf("online write overwrote manual edit: %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(raw) {
		t.Fatal("failed version check changed operator file")
	}
	fresh, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	fresh.Harnesses["mock"] = Harness{Adapter: "codex-acp", Command: "/derived/runtime/adapter"}
	if err := Save(path, fresh); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(path)
	if strings.Contains(string(raw), "/derived/runtime/adapter") {
		t.Fatal("saved runtime adapter command as an operator declaration")
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("saved adapter declaration cannot restart: %v", err)
	}
}

func TestCloneIntentSurvivesUnappliedFile(t *testing.T) {
	cfg, path, controller, _ := controllerFixture(t)
	cfg.Nodes = map[string]Node{"remote": {Addr: "127.0.0.1:1", Token: "test"}}
	p := cfg.Projects["p"]
	p.Workspaces = []ProjectWorkspace{{Node: "remote", Path: "/copy", Origin: "cloned", Source: "repository"}}
	cfg.Projects["p"] = p
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Reconcile(t.Context(), loaded); err != nil {
		t.Fatal(err)
	}
	got, _, _ := controller.Store.Get(t.Context(), "p")
	copy := got.Copies["remote"]
	if copy.Origin != project.OriginCloned || copy.Source != "repository" || copy.State != project.CopyProvisioning {
		t.Fatalf("unapplied clone became ready/adopted: %+v", copy)
	}
}
