package delegate

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
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

func boundDelegatePersistenceFixture(t *testing.T) (*world, *sql.DB, task.Task, task.Task) {
	t.Helper()
	w := newWorld(t)
	dir := t.TempDir()
	book, err := ledger.Open(dir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	tasks, err := task.OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	projects := project.Open(book)
	if err := projects.Declare(t.Context(), []project.Project{{ID: "p", Home: project.Home{Path: t.TempDir()}}}); err != nil {
		t.Fatal(err)
	}
	art := artifact.New(filepath.Join(t.TempDir(), "artifacts"), book, projects, artifact.LocalNodes{Dir: t.TempDir()})
	att := attempt.New(book)
	w.tasks = tasks
	w.attempts = att
	w.service.tasks = tasks
	w.service.workspaces = art
	w.service.SetLedger(att, art)
	parent := w.running(t, "codex")
	child, err := tasks.Spawn(parent.ID, task.Task{Goal: "committed reply", Member: "builder", Node: "node-a", Origin: "delegate:" + parent.ID, ProjectID: "p", Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tasks.Begin(child.ID, "builder", "node-a", "ns_committed"); err != nil {
		t.Fatal(err)
	}
	token, err := tasks.ExecutionToken(child.ID)
	if err != nil {
		t.Fatal(err)
	}
	record, err := att.Open(t.Context(), attempt.Spec{ID: "bound-before-task-write", Execution: &token, TaskID: child.ID, TurnID: "delegate/" + child.ID, Kind: attempt.KindDelegate, Project: "p", Node: "node-a", Agent: "builder", Harness: "mock", Scope: attempt.ScopePathSet, Workspace: project.Workspace{ID: "original-workspace", Project: "p", Node: "node-a", Path: child.Workspace, Kind: project.KindWorktree}})
	if err != nil {
		t.Fatal(err)
	}
	if err := tasks.BindAttempt(token, record.ID, record.TurnID); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []attempt.State{attempt.Prepared, attempt.Running, attempt.BindReady} {
		if _, err = att.Advance(t.Context(), record.ID, phase, "test", func(r *attempt.Record) { r.Session = "ns_committed" }); err != nil {
			t.Fatal(err)
		}
	}
	if err = att.MarkSessionSettled(t.Context(), record.ID, "test"); err != nil {
		t.Fatal(err)
	}
	result := agentmcp.DelegateResult{TaskID: child.ID, Agent: "builder", Node: "node-a", Outcome: "ok", Answer: "full committed reply"}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = att.Complete(t.Context(), record.ID, "test", attempt.Completion{Result: attempt.Result{Summary: result.Answer, Output: raw}, Usage: &attempt.Usage{Input: 31, Output: 9, Reported: true}}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return w, db, parent, child
}

func TestRetainedDelegateWaitsForBudgetStateAndResultDurabilityBeforeReportingDone(t *testing.T) {
	for _, phase := range []string{"budget", "state", "result"} {
		t.Run(phase, func(t *testing.T) {
			w, db, parent, child := boundDelegatePersistenceFixture(t)
			base := fmt.Sprintf(`$.tasks."%s"`, child.ID)
			condition := fmt.Sprintf("json_extract(NEW.data, '%s.budget.tokens.total') > 0", base)
			if phase == "state" {
				condition = fmt.Sprintf("json_extract(NEW.data, '%s.state') = 'done'", base)
			}
			if phase == "result" {
				condition = fmt.Sprintf("json_type(NEW.data, '%s.result') IS NOT NULL", base)
			}
			if _, err := db.Exec("CREATE TRIGGER reject_task_write BEFORE UPDATE OF data ON bindings WHEN NEW.kind = 'document' AND NEW.id = 'tasks' AND " + condition + " BEGIN SELECT RAISE(FAIL, 'task write unavailable'); END"); err != nil {
				t.Fatal(err)
			}
			service := recoveredDelegateService(t, w, w.sessions)
			notices := make(chan RecoveryQuestion, 1)
			service.SetRecoveryQuestion(func(_ context.Context, q RecoveryQuestion) (view.Answer, error) {
				notices <- q
				return view.Answer{Value: "wait"}, nil
			})
			var mu sync.Mutex
			delivered := 0
			service.SetDeliverer(func(context.Context, Delivery) error { mu.Lock(); delivered++; mu.Unlock(); return nil })
			if err := service.RecoverRetained(t.Context()); err != nil {
				t.Fatal(err)
			}
			select {
			case <-notices:
			case <-time.After(3 * time.Second):
				t.Fatal("failed task save had no recovery question")
			}
			until := time.Now().Add(time.Second)
			for time.Now().Before(until) {
				service.mu.Lock()
				entry := service.pending[child.ID]
				service.mu.Unlock()
				if entry == nil {
					break
				}
				time.Sleep(time.Millisecond)
			}
			result, err := service.Await(t.Context(), "chat", "codex", agentmcp.AwaitRequest{TaskID: child.ID, WaitSeconds: 1})
			if err != nil || result.State != "running" {
				t.Fatalf("unpersisted task was reported done: %+v %v", result, err)
			}
			stored, _ := w.tasks.Get(child.ID)
			if stored.Result != nil {
				t.Fatalf("unpersisted outcome leaked into task: %+v", stored)
			}
			mu.Lock()
			if delivered != 0 {
				t.Error("unpersisted result was delivered")
			}
			mu.Unlock()
			if _, err := db.Exec("DROP TRIGGER reject_task_write"); err != nil {
				t.Fatal(err)
			}
			if err := service.RecoverRetained(t.Context()); err != nil {
				t.Fatal(err)
			}
			stored = awaitDelegateResult(t, w.tasks, child.ID)
			if stored.State != task.StateDone || stored.Attempts[0].Open() || stored.Budget.Tokens.Total != 40 {
				t.Fatalf("task did not finish exactly once: %+v", stored)
			}
			charged, _ := w.tasks.Get(parent.ID)
			if charged.Budget.Tokens.Total != 40 {
				t.Fatalf("parent budget charged %d", charged.Budget.Tokens.Total)
			}
			service.RedeliverPending(t.Context())
			mu.Lock()
			defer mu.Unlock()
			if delivered != 1 {
				t.Fatalf("delivered %d times", delivered)
			}
		})
	}
}

func TestManagedDelegateSettlementMarkerFailureDoesNotDiscardNativeAnswer(t *testing.T) {
	w, db, _, _ := boundDelegatePersistenceFixture(t)
	lifetime, stop := context.WithCancel(t.Context())
	registry := execution.New(lifetime, w.tasks)
	w.service.SetExecution(registry)
	w.service.artifacts.SetExecution(registry)
	w.service.InlineWait = time.Millisecond
	t.Cleanup(func() {
		stop()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = registry.Shutdown(ctx)
	})
	sessions := &retainedDelegateSessions{attempts: w.attempts, tasks: w.tasks, entered: make(chan struct{}), answer: "complete native answer before marker failed", initialResult: true}
	w.service.sessions = sessions
	w.service.SetGate(nil)
	if _, err := db.Exec(`CREATE TRIGGER reject_settlement BEFORE UPDATE OF data ON operations WHEN NEW.kind = 'attempt' AND OLD.state = 'running' AND NEW.state = 'running' AND json_extract(OLD.data, '$.session_settled') = 0 AND json_extract(NEW.data, '$.session_settled') = 1 BEGIN SELECT RAISE(FAIL, 'marker storage unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	started, err := w.service.Start(t.Context(), "chat", "codex", agentmcp.DelegateRequest{Goal: "native work", Agent: "builder"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-sessions.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("native command did not execute")
	}
	until := time.Now().Add(3 * time.Second)
	for time.Now().Before(until) {
		w.service.mu.Lock()
		entry := w.service.pending[started.TaskID]
		w.service.mu.Unlock()
		if entry == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	stored, _ := w.tasks.Get(started.TaskID)
	if stored.State != task.StateRunning || stored.Result != nil || !stored.Attempts[0].Open() {
		t.Fatalf("settlement marker failure terminalized child: %+v", stored)
	}
	records, err := w.attempts.ForTask(t.Context(), started.TaskID)
	if err != nil || len(records) != 1 || records[0].State != attempt.Running {
		t.Fatalf("native result discarded: %+v %v", records, err)
	}
	if _, err := db.Exec("DROP TRIGGER reject_settlement"); err != nil {
		t.Fatal(err)
	}
	service := recoveredDelegateService(t, w, sessions)
	if err := service.RecoverRetained(t.Context()); err != nil {
		t.Fatal(err)
	}
	stored = awaitDelegateResult(t, w.tasks, started.TaskID)
	if stored.Result.Answer != sessions.answer || stored.State != task.StateDone {
		t.Fatalf("native answer not restored: %+v", stored)
	}
	sessions.mu.Lock()
	defer sessions.mu.Unlock()
	if sessions.prompts != 1 {
		t.Fatal("marker retry replayed prompt")
	}
}
