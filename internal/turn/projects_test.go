package turn

import (
	"context"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/datalevel"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/state"
)

// newCoordinator builds a coordinator whose projects mirror the catalog:
// one project per agent, named after it and homed on its node at a fresh
// directory, with the default agent's as the default project. Tests that
// care which directory a session opens in read it back with workspaceOf.
func newCoordinator(t *testing.T, catalog *agent.Catalog, store *state.Store, assembler *capability.Assembler, rt runtime, timeout time.Duration, opts ...testOption) *Coordinator {
	t.Helper()
	return newCoordinatorIn(t, nil, catalog, store, assembler, rt, timeout, opts...)
}

// newCoordinatorIn is newCoordinator with chosen directories per agent id;
// agents not in dirs get a fresh one.
func newCoordinatorIn(t *testing.T, dirs map[string]string, catalog *agent.Catalog, store *state.Store, assembler *capability.Assembler, rt runtime, timeout time.Duration, opts ...testOption) *Coordinator {
	t.Helper()
	var b testBuild
	for _, opt := range opts {
		opt(&b)
	}
	book := b.book
	if book == nil {
		book = testLedger(t)
	}
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
	return buildCoordinator(t, append([]testOption{onLedger(book), withDeps(func(d *Deps) {
		d.Catalog, d.Store, d.Assembler, d.Runtime, d.Timeout = catalog, store, assembler, rt, timeout
		d.Projects, d.DefaultProject = projects, catalog.Default().ID
	})}, opts...)...)
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
	if err := c.projects.Declare(context.Background(), []project.Project{{ID: "home", Level: datalevel.Restricted, Home: project.Home{Path: dir}}}); err != nil {
		t.Fatal(err)
	}
	c.homeProject = "home"
}

// restartCoordinator is the next gateway process: a new coordinator over
// the same ledger, stores and bindings the previous one used, with its own
// execution registry.
func restartCoordinator(t *testing.T, prev *Coordinator, catalog *agent.Catalog, store *state.Store, assembler *capability.Assembler, rt runtime, timeout time.Duration, opts ...testOption) *Coordinator {
	t.Helper()
	return buildCoordinator(t, append([]testOption{withDeps(func(d *Deps) {
		d.Catalog, d.Store, d.Assembler, d.Runtime, d.Timeout = catalog, store, assembler, rt, timeout
		d.Projects, d.DefaultProject, d.HomeProject = prev.projects, prev.defaultProject, prev.homeProject
		d.Attempts, d.Artifacts, d.Tasks, d.Node = prev.attempts, prev.artifacts, prev.tasks, prev.node
		d.Intents, d.Schedules, d.Memory = prev.intents, prev.schedules, prev.memory
	})}, opts...)...)
}
