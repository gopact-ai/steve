package exec

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/gopact/workflow"
	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/planner"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

type runWorld struct {
	mu    sync.Mutex
	book  *ledger.Ledger
	sup   *Supervisor
	plans *plan.Store
	tasks *task.Store
	plan  plan.Plan
	home  string
	calls map[string]int
}

func recoveryWorld(t *testing.T) *runWorld {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	projects := project.Open(book)
	home := t.TempDir()
	if err := projects.Declare(t.Context(), []project.Project{{ID: "p", Home: project.Home{Path: home}}}); err != nil {
		t.Fatal(err)
	}
	tasks, err := task.OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	work, err := tasks.Create(task.Task{ProjectID: "p", Member: "local"})
	if err != nil {
		t.Fatal(err)
	}
	plans, err := plan.OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	p, err := plans.Create(plan.Plan{TaskID: work.ID, ProjectID: "p", Goal: "recover", By: "test", Steps: []plan.Step{step("first", "first", []string{"basic"}), step("second", "second", []string{"basic"})}})
	if err != nil {
		t.Fatal(err)
	}
	art := artifact.New(t.TempDir(), book, projects, artifact.LocalNodes{Dir: t.TempDir()})
	att := attempt.New(book)
	w := &runWorld{book: book, plans: plans, tasks: tasks, plan: p, home: home, calls: map[string]int{}}
	// Serial runner counters use their own mutex; workflow branches still
	// execute in parallel and write separate workspaces.
	w.sup = NewSupervisor(planner.Rule{}, Deps{Attempts: att, Artifacts: art, Workspaces: art, Roster: testRoster(t, bothNodes()), Runner: runnerFunc(func(_ context.Context, req StepRequest) (plan.StepResult, error) {
		w.mu.Lock()
		w.calls[req.StepID]++
		w.mu.Unlock()
		if err := os.WriteFile(filepath.Join(req.Workspace, req.StepID), []byte(req.StepID), 0600); err != nil {
			return plan.StepResult{}, err
		}
		return plan.StepResult{Answer: req.StepID, Usage: &plan.Usage{Reported: true, Input: 10}}, nil
	})}, workflow.NewMemoryStore())
	w.sup.SetPlans(plans)
	w.sup.SetLedger(book, "test")
	w.sup.SetTasks(tasks)
	w.sup.SetExecution(execution.New(t.Context(), tasks))
	art.SetExecution(w.sup.deps.Executions)
	return w
}

func (w *runWorld) trigger(t *testing.T, sql string) {
	t.Helper()
	if err := w.book.Update(t.Context(), func(tx *ledger.Tx) error { _, err := tx.Exec(sql); return err }); err != nil {
		t.Fatal(err)
	}
}
func (w *runWorld) once(t *testing.T) {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.calls["first"] != 1 || w.calls["second"] != 1 {
		t.Fatalf("recovery reran work: %v", w.calls)
	}
}

func TestPlanRecoveryKeepsResponsibilityAcrossEveryCompletionBoundary(t *testing.T) {
	for _, point := range []string{"ready-to-land", "second-sink", "run-completed"} {
		t.Run(point, func(t *testing.T) {
			w := recoveryWorld(t)
			sql := `CREATE TRIGGER cut BEFORE UPDATE OF state ON operations WHEN NEW.kind='plan-run' AND NEW.state='landing' BEGIN SELECT RAISE(FAIL,'cut before landing'); END`
			if point == "second-sink" {
				sql = `CREATE TRIGGER cut BEFORE INSERT ON operations WHEN NEW.kind='landing' AND NEW.id LIKE '%/second/%' BEGIN SELECT RAISE(FAIL,'cut before second sink'); END`
			}
			if point == "run-completed" {
				sql = `CREATE TRIGGER cut BEFORE UPDATE OF state ON operations WHEN NEW.kind='plan-run' AND NEW.state='completed' BEGIN SELECT RAISE(FAIL,'cut after task completion'); END`
			}
			w.trigger(t, sql)
			if _, err := w.sup.Execute(t.Context(), w.plan); err == nil {
				t.Fatal("injected cut did not fail")
			}
			open, err := w.sup.OpenRuns(t.Context())
			if err != nil || len(open) != 1 {
				t.Fatalf("run forgotten: %v err=%v", open, err)
			}
			w.once(t)
			if point != "ready-to-land" {
				if err := os.WriteFile(filepath.Join(w.home, "first"), []byte("later user edit"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if point == "run-completed" {
				current, _ := w.tasks.Get(w.plan.TaskID)
				if current.State != task.StateDone {
					t.Fatal("fixture did not reach task-done boundary")
				}
			}
			w.trigger(t, "DROP TRIGGER cut")
			if err := w.sup.PrepareRecovery(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err := w.sup.Resume(t.Context(), open[0]); err != nil {
				t.Fatal(err)
			}
			w.once(t)
			if point != "ready-to-land" {
				data, _ := os.ReadFile(filepath.Join(w.home, "first"))
				if string(data) != "later user edit" {
					t.Fatal("already committed sink overwrote later edit")
				}
			}
			open, err = w.sup.OpenRuns(t.Context())
			if err != nil || len(open) != 0 {
				t.Fatalf("completed run still open: %v %v", open, err)
			}
			current, _ := w.tasks.Get(w.plan.TaskID)
			if current.State != task.StateDone {
				t.Fatal("task not completed after landing")
			}
			lands, err := w.sup.deps.Artifacts.Landings(t.Context(), "p")
			if err != nil || len(lands) != 2 {
				t.Fatalf("landing repeated: %d %v", len(lands), err)
			}
		})
	}
}

func TestPlanRunRegistrationFailureCannotStartExecution(t *testing.T) {
	w := recoveryWorld(t)
	w.trigger(t, `CREATE TRIGGER cut BEFORE INSERT ON operations WHEN NEW.kind='plan-run' BEGIN SELECT RAISE(FAIL,'registration rejected'); END`)
	if _, err := w.sup.Execute(t.Context(), w.plan); err == nil {
		t.Fatal("unregistered plan executed")
	}
	if len(w.calls) != 0 {
		t.Fatalf("work ran: %v", w.calls)
	}
}

func TestStoppedPlanCannotAcquireANewEpochToLandOldSinks(t *testing.T) {
	w := recoveryWorld(t)
	w.trigger(t, `CREATE TRIGGER cut BEFORE INSERT ON operations WHEN NEW.kind='landing' BEGIN SELECT RAISE(FAIL,'before sink'); END`)
	if _, err := w.sup.Execute(t.Context(), w.plan); err == nil {
		t.Fatal("cut missing")
	}
	open, _ := w.sup.OpenRuns(t.Context())
	if len(open) != 1 {
		t.Fatal("run missing")
	}
	if _, err := w.tasks.SetAside(w.plan.TaskID, task.StatePaused); err != nil {
		t.Fatal(err)
	}
	if _, err := w.tasks.Advance(w.plan.TaskID, task.StateRunning); err != nil {
		t.Fatal(err)
	}
	w.trigger(t, "DROP TRIGGER cut")
	_, err := w.sup.Resume(t.Context(), open[0])
	if !errors.Is(err, task.ErrExecutionStopped) {
		t.Fatalf("old result reused new authorization: %v", err)
	}
	for _, name := range []string{"first", "second"} {
		if _, err := os.Stat(filepath.Join(w.home, name)); !os.IsNotExist(err) {
			t.Fatal("stopped sink landed")
		}
	}
}

func TestPlanRecoveryCannotFollowAChangedHomeBeforeFirstLanding(t *testing.T) {
	w := recoveryWorld(t)
	w.trigger(t, `CREATE TRIGGER cut BEFORE UPDATE OF state ON operations WHEN NEW.kind='plan-run' AND NEW.state='landing' BEGIN SELECT RAISE(FAIL,'cut before landing registration'); END`)
	if _, err := w.sup.Execute(t.Context(), w.plan); err == nil {
		t.Fatal("cut missing")
	}
	open, _ := w.sup.OpenRuns(t.Context())
	if len(open) != 1 {
		t.Fatal("run missing")
	}
	newHome := t.TempDir()
	projects := project.Open(w.book)
	if err := projects.Declare(t.Context(), []project.Project{{ID: "p", Home: project.Home{Path: newHome}}}); err != nil {
		t.Fatal(err)
	}
	w.trigger(t, "DROP TRIGGER cut")
	if _, err := w.sup.Resume(t.Context(), open[0]); !errors.Is(err, ErrRecovery) {
		t.Fatalf("recovery followed new home: %v", err)
	}
	w.once(t)
	for _, home := range []string{newHome, w.home} {
		for _, name := range []string{"first", "second"} {
			if _, err := os.Stat(filepath.Join(home, name)); !os.IsNotExist(err) {
				t.Fatal("old result landed after reassignment")
			}
		}
	}
}

func TestRunDriverRenewsAndFencesAReplacedOwner(t *testing.T) {
	w := recoveryWorld(t)
	w.sup.driverTTL = 150 * time.Millisecond
	rec, err := w.sup.opened(t.Context(), w.plan)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan context.Context, 1)
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := w.sup.runOwner(t.Context(), rec, func(ctx context.Context, current RunRecord) (Outcome, error) {
			entered <- ctx
			<-release
			return Outcome{}, w.sup.saveRun(context.WithoutCancel(ctx), &current, RunLanding)
		})
		done <- err
	}()
	ownerCtx := <-entered
	time.Sleep(3 * w.sup.driverTTL)
	if _, err := w.sup.runOwner(t.Context(), rec, func(context.Context, RunRecord) (Outcome, error) {
		t.Error("second driver entered")
		return Outcome{}, nil
	}); !errors.Is(err, ledger.ErrHeld) {
		t.Fatalf("driver lease expired: %v", err)
	}
	if err := w.book.Invalidate(t.Context(), "plan-driver:"+rec.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ownerCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("lost driver was not cancelled")
	}
	close(release)
	if err := <-done; !errors.Is(err, ledger.ErrStale) {
		t.Fatalf("stale driver changed run phase: %v", err)
	}
}

func TestRevisionLimitComesFromPersistedPlanRevision(t *testing.T) {
	w := recoveryWorld(t)
	for range MaxRevisions {
		if _, err := w.plans.Revise(w.plan.ID, w.plan.Steps, "test", "revision"); err != nil {
			t.Fatal(err)
		}
	}
	latest, _ := w.plans.Latest(w.plan.ID)
	planner := &scriptedPlanner{}
	w.sup.planner = planner
	_, err := w.sup.continueFrom(t.Context(), latest, Outcome{}, ErrExhausted{StepID: "first", Attempts: 5, Cause: errors.New("failed")})
	if err == nil || len(planner.asked) != 0 {
		t.Fatalf("restart reset revision budget: calls=%d err=%v", len(planner.asked), err)
	}
}
