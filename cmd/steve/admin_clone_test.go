package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/roster"
)

func configuredCloneFixture(t *testing.T) *fleetAdmin {
	t.Helper()
	a, book := projectAdminFixture(t)
	a.attempts = attempt.New(book)
	a.fleet = roster.New(a.catalog)
	p := a.cfg.Projects["remove"]
	p.Workspaces = []config.ProjectWorkspace{{Node: "remote", Path: "/held-clone", Origin: "cloned", Source: "source"}}
	a.cfg.Projects["remove"] = p
	if err := config.Save(a.path, a.cfg); err != nil {
		t.Fatal(err)
	}
	if err := (config.ProjectController{Store: a.projects}).Reconcile(t.Context(), a.cfg); err != nil {
		t.Fatal(err)
	}
	return a
}

func awaitCloneState(t *testing.T, a *fleetAdmin, state project.CloneState) project.CloneOperation {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		ops, err := a.projects.CloneOperations(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if len(ops) == 1 && ops[0].State == state {
			return ops[0]
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("clone did not reach %s", state)
	return project.CloneOperation{}
}

func TestRunningCloneBlocksRemovalRetirementAndReassignment(t *testing.T) {
	a := configuredCloneFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	a.cloneFiles = func(ctx context.Context, _ string, _ nodewire.FileRequest) (string, error) {
		close(entered)
		select {
		case <-release:
			return "", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if err := a.ResumeProjectCopies(t.Context()); err != nil {
		t.Fatal(err)
	}
	<-entered
	op := awaitCloneState(t, a, "running")
	if op.Lease.Key != project.CopyID("remove", "remote") || op.Lease.Holder != op.ID {
		t.Fatal("clone does not hold the interactive workspace fence")
	}
	for _, remove := range []func() error{
		func() error { return a.RemoveWorkspace(t.Context(), "remove", "remote") },
		func() error { return a.RemoveProject(t.Context(), "remove") },
	} {
		err := remove()
		if !errors.Is(err, consoleapi.ErrBusy) && !errors.Is(err, project.ErrCloneIsolated) {
			t.Fatalf("running clone lost ownership: %v", err)
		}
	}
	candidate := config.CloneProjects(a.cfg)
	delete(candidate.Projects, "remove")
	candidate.Projects["replacement"] = config.Project{Home: config.ProjectHome{Node: "remote", Path: "/held-clone"}}
	if err := (config.ProjectController{Store: a.projects}).Commit(t.Context(), candidate, func() error { t.Fatal("isolated path reached file commit"); return nil }); !errors.Is(err, project.ErrCloneIsolated) {
		t.Fatalf("path reassigned while clone writes: %v", err)
	}
	close(release)
	awaitCloneState(t, a, "succeeded")
	if err := a.RemoveWorkspace(t.Context(), "remove", "remote"); err != nil {
		t.Fatalf("confirmed clone did not release workspace: %v", err)
	}
}

func TestTimedOutRemoteCloneRecordsUnknownWithFreshCleanupContext(t *testing.T) {
	a := configuredCloneFixture(t)
	a.cloneTimeout = 20 * time.Millisecond
	a.cloneLeaseTTL = 100 * time.Millisecond
	var calls atomic.Int32
	a.cloneFiles = func(ctx context.Context, _ string, _ nodewire.FileRequest) (string, error) {
		calls.Add(1)
		<-ctx.Done()
		return "", ctx.Err()
	}
	if err := a.ResumeProjectCopies(t.Context()); err != nil {
		t.Fatal(err)
	}
	op := awaitCloneState(t, a, "unconfirmed")
	if op.Error == "" || op.Evidence == "" {
		t.Fatal("timeout quarantine lacks cause/evidence")
	}
	p, _, err := a.projects.Get(t.Context(), "remove")
	if err != nil || p.Copies["remote"].State != project.CopyFailed {
		t.Fatalf("expired operation context prevented cleanup: %v", err)
	}
	if err := a.ResumeProjectCopies(t.Context()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("unknown remote clone automatically replayed")
	}
	time.Sleep(120 * time.Millisecond)
	if err := a.RemoveWorkspace(t.Context(), "remove", "remote"); !errors.Is(err, project.ErrCloneIsolated) {
		t.Fatalf("lease expiry cleared unknown physical ownership: %v", err)
	}
	if err := a.projects.ConfirmCloneStopped(t.Context(), op.ID, "operator", "test fixture confirms file worker returned"); err != nil {
		t.Fatal(err)
	}
	if err := a.RemoveWorkspace(t.Context(), "remove", "remote"); err != nil {
		t.Fatalf("explicit stop confirmation did not permit release: %v", err)
	}
}
