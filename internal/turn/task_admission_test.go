package turn

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/view"
)

func TestTaskPersistenceFailureRefusesExecutionAndCanRetry(t *testing.T) {
	for _, stage := range []string{"create", "begin", "close-previous-project"} {
		t.Run(stage, func(t *testing.T) {
			runner := &fakeRunner{reply: "done"}
			c, tasks, book := taskCoordinatorBook(t, runner)
			if stage != "create" {
				projectID := "codex"
				if stage == "close-previous-project" {
					projectID = "previous-project"
				}
				if _, err := tasks.Create(task.Task{Goal: "existing goal", Channel: "chat", Member: "codex", ProjectID: projectID}); err != nil {
					t.Fatal(err)
				}
			}
			before, err := json.Marshal(tasks.List(""))
			if err != nil {
				t.Fatal(err)
			}
			if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
				_, err := tx.Exec(`CREATE TRIGGER reject_task_admission BEFORE INSERT ON bindings
					WHEN NEW.kind = 'task-store' AND NEW.id = 'state'
					BEGIN SELECT RAISE(ABORT, 'task admission unavailable'); END`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			_, err = handle(c, t.Context(), "perform work")
			if err == nil || !strings.Contains(err.Error(), "task admission unavailable") {
				t.Errorf("task storage failure was hidden: %v", err)
			}
			if got := runner.seen(); len(got) != 0 {
				t.Errorf("unadmitted work reached the agent: %v", got)
			}
			manager := c.runtime.(*fakeManager)
			if len(manager.opened) != 0 {
				t.Errorf("unadmitted work opened native sessions: %v", manager.opened)
			}
			after, err := json.Marshal(tasks.List(""))
			if err != nil || string(before) != string(after) {
				t.Errorf("rejected admission changed task state: before=%s after=%s err=%v", before, after, err)
			}
			if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
				_, err := tx.Exec(`DROP TRIGGER reject_task_admission`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := handle(c, t.Context(), "perform work"); err != nil {
				t.Fatalf("retry after storage recovery: %v", err)
			}
			if got := runner.seen(); len(got) != 1 {
				t.Fatalf("retry should execute exactly once, got %d prompts", len(got))
			}
		})
	}
}

func TestWorkspacePreparationCannotBypassTaskRevocation(t *testing.T) {
	for _, action := range []string{"pause", "cancel", "pause-resume"} {
		t.Run(action, func(t *testing.T) {
			runner := &fakeRunner{reply: "done"}
			c, _ := completionCoordinator(t, runner)
			if _, err := handle(c, t.Context(), "first"); err != nil {
				t.Fatal(err)
			}
			old, ok := c.tasks.Active("chat", "codex", "")
			if !ok {
				t.Fatal("first turn has no task")
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			entered, resume := make(chan struct{}), make(chan struct{})
			var once sync.Once
			release := func() { once.Do(func() { close(resume) }) }
			defer release()
			done := make(chan error, 1)
			go func() {
				_, err := c.Handle(ctx, Request{ConversationID: "chat", Input: "second", MessageID: "second-message", OnStage: func(stage view.Stage) {
					if stage == view.StageWorkspace {
						close(entered)
						select {
						case <-resume:
						case <-ctx.Done():
						}
					}
				}})
				done <- err
			}()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("turn never reached workspace preparation")
			}
			stopState := task.StatePaused
			if action == "cancel" {
				stopState = task.StateCancelled
			}
			ids, err := c.tasks.SetAside(old.ID, stopState)
			if err != nil {
				t.Fatal(err)
			}
			waiting := c.executions.Stop(ids, task.ErrExecutionStopped)
			if action == "pause-resume" {
				if _, err := c.tasks.Advance(old.ID, task.StateRunning); err != nil {
					t.Fatal(err)
				}
			}
			release()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) && !errors.Is(err, task.ErrExecutionStopped) {
					t.Fatalf("revoked preparation was admitted: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("revoked turn did not finish")
			}
			if err := waiting.Wait(ctx); err != nil {
				t.Fatal(err)
			}
			current, _ := c.tasks.Get(old.ID)
			if len(runner.seen()) != 1 || len(c.tasks.List("chat")) != 1 || current.Budget.Turns != old.Budget.Turns || len(current.Attempts) != len(old.Attempts) {
				t.Fatalf("revoked preparation created or charged new work: %+v prompts=%v", current, runner.seen())
			}
			if action == "pause-resume" {
				if _, err := handle(c, t.Context(), "a new message after resume"); err != nil {
					t.Fatalf("fresh input could not use the resumed epoch: %v", err)
				}
				if len(runner.seen()) != 2 {
					t.Fatal("fresh input did not execute exactly once")
				}
			}
		})
	}
}

func TestProjectSwitchSetsAsideUnfinishedTasksBeforeAdmittingNewWork(t *testing.T) {
	for _, previous := range []task.State{task.StateFailed, task.StateBlocked} {
		for _, entry := range []string{"command", "interrupted-switch"} {
			for _, refuseWrite := range []bool{false, true} {
				name := string(previous) + "/" + entry
				if refuseWrite {
					name += "/storage-failure"
				}
				t.Run(name, func(t *testing.T) {
					book, err := ledger.Open(t.TempDir(), ledger.Options{})
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = book.Close() })
					tasks, err := task.OpenLedger(book)
					if err != nil {
						t.Fatal(err)
					}
					sessions, err := state.OpenLedger(book)
					if err != nil {
						t.Fatal(err)
					}
					projects := project.Open(book)
					nextDir := t.TempDir()
					if err := projects.Declare(t.Context(), []project.Project{
						{ID: "first", Home: project.Home{Path: t.TempDir()}},
						{ID: "next", Home: project.Home{Path: nextDir}},
					}); err != nil {
						t.Fatal(err)
					}
					catalog, err := agent.NewCatalog(map[string]agent.Config{"worker": {Harness: "test", Default: true}})
					if err != nil {
						t.Fatal(err)
					}
					runner := &fakeRunner{reply: "done"}
					manager := &fakeManager{runners: map[string]*fakeRunner{"test": runner}}
					c := buildCoordinator(t, withDeps(func(d *Deps) {
						d.Catalog, d.Store, d.Assembler, d.Runtime, d.Timeout = catalog, sessions, capability.NewAssembler(nil), manager, time.Minute
						d.Tasks, d.Node = tasks, "hub"
						d.Projects, d.DefaultProject = projects, "first"
					}), onLedger(book))
					if _, err := handle(c, t.Context(), "old project work"); err != nil {
						t.Fatal(err)
					}
					old, ok := tasks.Active("chat", "worker", "")
					if !ok {
						t.Fatal("first turn has no task")
					}
					child, err := tasks.Spawn(old.ID, task.Task{Goal: "old project subtask", Member: "helper", ProjectID: "first"})
					if err != nil {
						t.Fatal(err)
					}
					childScope, err := c.executions.Begin(t.Context(), execution.Key{TaskID: child.ID, InstanceID: "old-child"})
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { childScope.Finish(nil) })
					old, err = tasks.Advance(old.ID, previous)
					if err != nil {
						t.Fatal(err)
					}
					if refuseWrite {
						if _, err := book.DB().Exec(`CREATE TRIGGER refuse_old_task_release BEFORE INSERT ON bindings
							WHEN NEW.kind = 'task-store' AND NEW.id = 'state'
							BEGIN SELECT RAISE(ABORT, 'task release unavailable'); END`); err != nil {
							t.Fatal(err)
						}
					}
					if entry == "command" {
						if _, err := handle(c, t.Context(), "/project use next"); err != nil {
							t.Fatal(err)
						}
					} else {
						if _, err := projects.Bind(t.Context(), "chat", "next", "owner"); err != nil {
							t.Fatal(err)
						}
						if err := sessions.ArchiveSession("chat", "worker", time.Now().UTC().Format(time.RFC3339)); err != nil {
							t.Fatal(err)
						}
					}
					if refuseWrite {
						if _, err := handle(c, t.Context(), "new project work"); err == nil || !strings.Contains(err.Error(), "task release unavailable") {
							t.Fatalf("task release failure was hidden: %v", err)
						}
						current, _ := tasks.Get(old.ID)
						if !reflect.DeepEqual(current, old) || len(runner.seen()) != 1 || len(manager.opened) != 1 {
							t.Fatalf("rejected release changed old work or executed new work: %+v", current)
						}
						currentChild, _ := tasks.Get(child.ID)
						if !reflect.DeepEqual(currentChild, child) || childScope.Context().Err() != nil {
							t.Fatal("failed durable release stopped the child anyway")
						}
						if _, err := book.DB().Exec(`DROP TRIGGER refuse_old_task_release`); err != nil {
							t.Fatal(err)
						}
					}
					for range 2 {
						if _, err := handle(c, t.Context(), "new project work"); err != nil {
							t.Fatalf("new project could not execute: %v", err)
						}
					}
					retired, _ := tasks.Get(old.ID)
					if retired.State != task.StatePaused || retired.ExecutionEpoch <= old.ExecutionEpoch || retired.ProjectID != old.ProjectID ||
						!reflect.DeepEqual(retired.Attempts, old.Attempts) || !reflect.DeepEqual(retired.Budget, old.Budget) {
						t.Fatalf("old task was completed, continued or lost its history: %+v", retired)
					}
					select {
					case <-childScope.Context().Done():
					case <-time.After(5 * time.Second):
						t.Fatal("old project's child observer was not stopped")
					}
					if child, _ := tasks.Get(child.ID); child.State != task.StatePaused || !errors.Is(tasks.CheckExecution(*childScope.Token()), task.ErrExecutionStopped) {
						t.Fatal("old project's child execution kept its authorization")
					}
					current, ok := tasks.Active("chat", "worker", "")
					if !ok || current.ID == old.ID || current.ProjectID != "next" || current.Budget.Turns != 2 || len(tasks.List("chat")) != 3 {
						t.Fatalf("new work did not keep its own project and accounting: %+v", current)
					}
					if len(runner.seen()) != 3 || len(manager.workdirs) != 3 || manager.workdirs[1] != nextDir || manager.workdirs[2] != nextDir {
						t.Fatalf("new work did not execute exactly twice in its project: %v", manager.workdirs)
					}
				})
			}
		}
	}
}
