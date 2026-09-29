package delegate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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
	return executionWorldOn(t, "", localNodes)
}

// executionWorldOn is executionWorld whose project's main directory is
// on node, and whose artifact operations run on nodes.
func executionWorldOn(t *testing.T, node string, nodes func(artifact.LocalNodes) artifact.Nodes) (*world, *execution.Registry) {
	w := newWorld(t)
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	tasks, err := task.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	projects := project.Open(book)
	home := t.TempDir()
	if err := projects.Declare(t.Context(), []project.Project{{ID: "p", Home: project.Home{Node: node, Path: home}}}); err != nil {
		t.Fatal(err)
	}
	art := artifact.New(filepath.Join(t.TempDir(), "artifacts"), book, projects, nodes(artifact.LocalNodes{Dir: t.TempDir()}))
	att := attempt.New(book)
	r := execution.New(t.Context(), tasks)
	w.book = book
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

// A child stopped just as the silence cuts off its landing in its
// in-place parent's directory is not queued to land. The context the
// queue is written on no longer ends with the child's; the stop is in the
// ledger before the queue is written, and the queue checks it there.
func TestAChildStoppedAsItsLandingIsCutIsNotQueued(t *testing.T) {
	hang := &hangingHome{reached: make(chan struct{})}
	w, r := executionWorldOn(t, "node-a", func(n artifact.LocalNodes) artifact.Nodes {
		hang.LocalNodes = n
		return hang
	})
	hang.home = w.home
	parent := w.running(t, "codex")
	token, err := w.tasks.ExecutionToken(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.attempts.Open(t.Context(), attempt.Spec{Execution: &token, TaskID: parent.ID, Kind: attempt.KindChat, Project: "p", Node: "node-a", Agent: "codex", Harness: "mock",
		Workspace: project.Workspace{ID: "canonical:p", Project: "p", Node: "node-a", Path: w.home, Kind: project.KindCanonical}, Scope: attempt.ScopeUnrestricted}); err != nil {
		t.Fatal(err)
	}
	stopped := make(chan execution.WaitSet, 1)
	hang.cut = func() {
		ids, err := w.tasks.SetAside(parent.ID, task.StateCancelled)
		if err != nil {
			t.Errorf("set the parent aside: %v", err)
		}
		stopped <- r.Stop(ids, task.ErrExecutionStopped)
	}
	// Room for the setup under the race detector, as for the other
	// children cut for silence.
	const silence = 3 * time.Second
	w.service.MaxSilence = silence

	first, release := startBlocked(t, w, "codex")
	w.service.mu.Lock()
	entry := w.service.pending[first.TaskID]
	w.service.mu.Unlock()
	child, ok := w.tasks.Get(first.TaskID)
	if entry == nil || !ok || child.Workspace == "" {
		t.Fatalf("child unknown: tracked=%v task=%+v", entry != nil, child)
	}
	if err := os.WriteFile(filepath.Join(child.Workspace, "notes.md"), []byte("by the child\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hang.armed.Store(true)
	release()
	select {
	case <-hang.reached:
	case <-time.After(3 * silence):
		t.Fatal("the result was not landed under the parent's lock")
	}
	select {
	case <-entry.done:
	case <-time.After(3 * silence):
		t.Fatal("the child still runs once its landing was cut")
	}
	select {
	case waiting := <-stopped:
		if err := waiting.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("the child ended before its landing was cut")
	}

	res, err := w.service.settle(w.service.snapshot(entry))
	refs := strings.Join(res.Refs, " | ")
	if res.State != task.StateCancelled || !strings.Contains(refs, "silent past the idle timeout") || strings.Contains(refs, "queued to land") {
		t.Fatalf("parent reads state=%s outcome=%s refs=%q err=%v; want a cancelled child whose landing the silence cut, not queued", res.State, res.Outcome, refs, err)
	}
	if child, _ = w.tasks.Get(first.TaskID); child.State != task.StateCancelled {
		t.Fatalf("child task = %s result=%+v", child.State, child.Result)
	}
	if pending, err := w.book.Bindings(t.Context(), "pending-landing"); err != nil || len(pending) != 0 {
		t.Fatalf("stopped child queued its landing: %v err=%v", pending, err)
	}
}

// The child's canonical region comes from the parent's project. A project
// that is absent has no home region; one that cannot be read is an error,
// not a project without a home region.
func TestHomeRegionReportsAnUnreadableProject(t *testing.T) {
	w, _ := executionWorld(t)
	if region, err := w.service.homeRegion(t.Context(), "absent"); err != nil || region != "" {
		t.Fatalf("absent project: region %q, err %v", region, err)
	}
	if err := w.book.PutBinding(t.Context(), "project", "p", "not a project"); err != nil {
		t.Fatal(err)
	}
	if region, err := w.service.homeRegion(t.Context(), "p"); err == nil {
		t.Fatalf("unreadable project gave region %q and no error", region)
	}
}
