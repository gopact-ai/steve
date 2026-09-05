package turn

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/state"
)

// newCoordinator builds a coordinator whose projects mirror the catalog:
// one project per agent, named after it and homed on its node at a fresh
// directory, with the default agent's as the default project. Tests that
// care which directory a session opens in read it back with workspaceOf.
func newCoordinator(t *testing.T, catalog *agent.Catalog, store *state.Store, assembler *capability.Assembler, rt runtime, timeout time.Duration) *Coordinator {
	t.Helper()
	return newCoordinatorIn(t, nil, catalog, store, assembler, rt, timeout)
}

// newCoordinatorIn is newCoordinator with chosen directories per agent id;
// agents not in dirs get a fresh one.
func newCoordinatorIn(t *testing.T, dirs map[string]string, catalog *agent.Catalog, store *state.Store, assembler *capability.Assembler, rt runtime, timeout time.Duration) *Coordinator {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	projects := project.Open(book)
	var declared []project.Project
	for _, a := range catalog.List() {
		dir := dirs[a.ID]
		if dir == "" {
			dir = t.TempDir()
		}
		declared = append(declared, project.Project{ID: a.ID, Home: project.Home{Node: a.Node, Path: dir}})
	}
	if err := projects.Declare(context.Background(), declared); err != nil {
		t.Fatal(err)
	}
	c := New(catalog, store, assembler, rt, timeout)
	c.SetProjects(projects, catalog.Default().ID, "")
	c.SetAttempts(attempt.New(book))
	c.SetArtifacts(artifact.New(filepath.Join(t.TempDir(), "artifacts"), book, projects, artifact.LocalNodes{Dir: t.TempDir()}))
	return c
}

// workspaceOf is the directory the agent's own project is homed at.
func workspaceOf(t *testing.T, c *Coordinator, agentID string) string {
	t.Helper()
	p, ok, err := c.projects.Get(context.Background(), agentID)
	if err != nil || !ok {
		t.Fatalf("project %s: ok=%v err=%v", agentID, ok, err)
	}
	return p.Home.Path
}

// useHome declares Steve's home directory as the owner's project, as the
// gateway does at boot.
func useHome(t *testing.T, c *Coordinator, dir string) {
	t.Helper()
	if err := c.projects.Declare(context.Background(), []project.Project{{ID: "home", Level: project.LevelRestricted, Home: project.Home{Path: dir}}}); err != nil {
		t.Fatal(err)
	}
	c.homeProject = "home"
}

// restartCoordinator is the next gateway process: a new coordinator over
// the same ledger, projects and bindings the previous one used.
func restartCoordinator(t *testing.T, prev *Coordinator, catalog *agent.Catalog, store *state.Store, assembler *capability.Assembler, rt runtime, timeout time.Duration) *Coordinator {
	t.Helper()
	c := New(catalog, store, assembler, rt, timeout)
	c.SetProjects(prev.projects, prev.defaultProject, prev.homeProject)
	c.SetAttempts(prev.attempts)
	c.SetArtifacts(prev.artifacts)
	return c
}
