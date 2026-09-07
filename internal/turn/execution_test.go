package turn

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
)

func TestTaskScopeStopsChatAndWaitsForDurableCleanup(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	projects := project.Open(book)
	if err := projects.Declare(t.Context(), []project.Project{{ID: "p", Home: project.Home{Path: t.TempDir()}}}); err != nil {
		t.Fatal(err)
	}
	tasks, err := task.OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := state.OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Default: true}})
	runner := &fakeRunner{reply: "ok", started: make(chan struct{}), done: make(chan struct{})}
	c := New(catalog, sessions, capability.NewAssembler(nil), &fakeManager{runners: map[string]*fakeRunner{"codex": runner}}, time.Minute)
	c.SetProjects(projects, "p", "")
	c.SetTasks(tasks, "hub")
	c.SetAttempts(attempt.New(book))
	artifacts := artifact.New(filepath.Join(t.TempDir(), "artifacts"), book, projects, artifact.LocalNodes{Dir: t.TempDir()})
	c.SetArtifacts(artifacts)
	registry := execution.New(t.Context(), tasks)
	c.SetExecution(registry)
	artifacts.SetExecution(registry)
	finished := make(chan error, 1)
	go func() { _, err := handle(c, t.Context(), "work"); finished <- err }()
	<-runner.started
	result, err := handle(c, t.Context(), "/tasks pause 1")
	if err != nil || !strings.Contains(result.Text, "paused") {
		t.Fatalf("pause=%+v %v", result, err)
	}
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("chat result=%v", err)
	}
	tracked, _ := tasks.Get("1")
	if tracked.State != task.StatePaused || len(tracked.Attempts) != 1 || tracked.Attempts[0].Open() {
		t.Fatalf("task cleanup=%+v", tracked)
	}
	records, err := c.attempts.ForTask(t.Context(), "1")
	if err != nil || len(records) != 1 || records[0].State != attempt.Failed {
		t.Fatalf("attempt cleanup=%+v %v", records, err)
	}
}
