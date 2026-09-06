package delegate

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/view"
)

func executionWorld(t *testing.T) (*world, *execution.Registry) {
	w := newWorld(t)
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	tasks, err := task.OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	projects := project.Open(book)
	home := t.TempDir()
	if err := projects.Declare(t.Context(), []project.Project{{ID: "p", Home: project.Home{Path: home}}}); err != nil {
		t.Fatal(err)
	}
	art := artifact.New(filepath.Join(t.TempDir(), "artifacts"), book, projects, artifact.LocalNodes{Dir: t.TempDir()})
	att := attempt.New(book)
	r := execution.New(t.Context(), tasks)
	w.tasks = tasks
	w.service.tasks = tasks
	w.service.workspaces = art
	w.service.SetLedger(att, art)
	w.attempts = att
	w.home = home
	art.SetExecution(r)
	w.service.SetExecution(r)
	w.service.InlineWait = time.Millisecond
	return w, r
}

func TestTaskStopReachesDetachedDelegateAndRejectsLateSuccess(t *testing.T) {
	w, r := executionWorld(t)
	parent := w.running(t, "codex")
	entered := make(chan context.Context, 1)
	release := make(chan struct{})
	w.sessions.run = func(ctx context.Context, p func(view.Progress)) (string, error) {
		entered <- ctx
		<-release
		p(view.Progress{Usage: view.Usage{Reported: true, InputTokens: 5}})
		return "late success", nil
	}
	request, cancel := context.WithCancel(t.Context())
	child, err := w.service.Start(request, "chat", "codex", agentmcp.DelegateRequest{Goal: "work", Agent: "builder"})
	if err != nil {
		t.Fatal(err)
	}
	childCtx := <-entered
	cancel()
	if childCtx.Err() != nil {
		t.Fatal("request cancellation killed accepted child")
	}
	ids, err := w.tasks.SetAside(parent.ID, task.StateCancelled)
	if err != nil {
		t.Fatal(err)
	}
	waiting := r.Stop(ids, task.ErrExecutionStopped)
	if childCtx.Err() == nil {
		t.Fatal("task stop did not reach detached child")
	}
	close(release)
	if err := waiting.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	stored, _ := w.tasks.Get(child.TaskID)
	if stored.State != task.StateCancelled {
		t.Fatalf("late success replaced cancelled state: %s", stored.State)
	}
	if _, found, err := w.service.artifacts.Resolve(t.Context(), "steve/"+child.TaskID+"/result"); err != nil || found {
		t.Fatalf("late child bound: %v %v", found, err)
	}
	records, err := w.attempts.ForTask(t.Context(), child.TaskID)
	if err != nil || len(records) != 1 || records[0].State != attempt.Failed || records[0].Usage == nil || records[0].Usage.Input != 5 {
		t.Fatalf("cancellation did not settle spend: %+v %v", records, err)
	}
}
