package exec

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

func TestPlanRunRegistrationRejectsDeletedTaskAuthority(t *testing.T) {
	w := recoveryWorld(t)
	token, err := w.tasks.ExecutionToken(w.plan.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	w.plan.Execution = &token
	// A late caller may still hold the typed plan after conversation deletion.
	if _, err := w.tasks.DeleteChannel(t.Context(), "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := w.sup.Execute(t.Context(), w.plan); !errors.Is(err, task.ErrExecutionStopped) {
		t.Fatalf("deleted authority error = %v", err)
	}
	if op, found, err := w.book.Operation(t.Context(), planRunID(w.plan.ID)); err != nil || found {
		t.Fatalf("late execution registered an orphan run: %+v found=%t err=%v", op, found, err)
	}
	if len(w.calls) != 0 {
		t.Fatal("deleted task executed")
	}
}

// Budget exhaustion retains responsibility until an explicit discard, rather
// than confusing a rejected admission with a still-running native writer.
func TestBudgetBlockedPlanRunRetiresWithItsTask(t *testing.T) {
	w := recoveryWorld(t)
	w.sup.deps.Budget = budgetFunc(func(string) (int, time.Time, error) {
		return 0, time.Time{}, errors.New("task budget exhausted: turns")
	})
	_, err := w.sup.Execute(t.Context(), w.plan)
	var noBudget ErrNoBudget
	if !errors.As(err, &noBudget) {
		t.Fatalf("expected budget refusal: %v", err)
	}
	open, err := w.sup.OpenRuns(t.Context())
	if err != nil || len(open) != 1 || open[0].Phase != RunExecuting {
		t.Fatalf("budget refusal lost responsibility: %+v %v", open, err)
	}
	if len(w.calls) != 0 {
		t.Fatal("exhausted budget reached runner")
	}
	if _, err := w.tasks.SetAside(w.plan.TaskID, task.StateCancelled); err != nil {
		t.Fatal(err)
	}
	if _, err := w.tasks.DeleteChannelWith(t.Context(), "", attempt.CheckTaskDeletionTx, DiscardTaskRunsTx); err != nil {
		t.Fatal(err)
	}
	rec, found, err := w.sup.loadRun(t.Context(), open[0].ID)
	if err != nil || !found || rec.Phase != RunCompleted || rec.Outcome != "discarded" || rec.Error != open[0].Error {
		t.Fatalf("budget refusal not honestly retired: %+v %v", rec, err)
	}
	if _, err := w.sup.Resume(t.Context(), open[0]); err == nil {
		t.Fatal("discarded budget failure replayed as success")
	}
}

func TestPlanRunRegistrationRejectsAnotherTasksTokenForDeletedOwner(t *testing.T) {
	for _, route := range []string{"plan-token", "context-token"} {
		t.Run(route, func(t *testing.T) {
			w := recoveryWorld(t)
			other, err := w.tasks.Create(task.Task{Channel: "console:other-owner", ProjectID: "p"})
			if err != nil {
				t.Fatal(err)
			}
			token, err := w.tasks.ExecutionToken(other.ID)
			if err != nil {
				t.Fatal(err)
			}
			ctx := t.Context()
			if route == "plan-token" {
				w.plan.Execution = &token
			} else {
				scope, err := w.sup.deps.Executions.BeginAccepted(ctx, execution.Key{TaskID: other.ID, InstanceID: "unrelated"}, &token)
				if err != nil {
					t.Fatal(err)
				}
				defer scope.Finish(nil)
				ctx = scope.Context()
			}
			if _, err := w.tasks.DeleteChannel(t.Context(), "", nil); err != nil {
				t.Fatal(err)
			}
			if _, err := w.sup.Execute(ctx, w.plan); err == nil {
				t.Fatal("another task's token authorized a deleted owner")
			}
			if op, found, err := w.book.Operation(t.Context(), planRunID(w.plan.ID)); err != nil || found {
				t.Fatalf("cross-owner token registered orphan run: %+v found=%t err=%v", op, found, err)
			}
			if len(w.calls) != 0 {
				t.Fatal("cross-owner registration reached runner")
			}
			if err := w.tasks.CheckExecution(token); err != nil {
				t.Fatalf("rejection revoked unrelated task permission: %v", err)
			}
		})
	}
}

func TestPlanRunRegistrationCannotBorrowAnotherPlanOwner(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "retained"}[existing], func(t *testing.T) {
			w := recoveryWorld(t)
			var before ledger.Operation
			if existing {
				if _, err := w.sup.opened(t.Context(), w.plan); err != nil {
					t.Fatal(err)
				}
				before, _, _ = w.book.Operation(t.Context(), planRunID(w.plan.ID))
			}
			other, err := w.tasks.Create(task.Task{Channel: "other", ProjectID: "p"})
			if err != nil {
				t.Fatal(err)
			}
			token, err := w.tasks.ExecutionToken(other.ID)
			if err != nil {
				t.Fatal(err)
			}
			aliased := w.plan
			aliased.TaskID, aliased.Execution = other.ID, &token
			if _, err := w.sup.Execute(t.Context(), aliased); err == nil {
				t.Fatal("stored plan was executed as another task")
			}
			op, found, err := w.book.Operation(t.Context(), planRunID(w.plan.ID))
			if err != nil || found != existing || existing && !reflect.DeepEqual(before, op) {
				t.Fatalf("owner rejection changed durable run: %+v found=%t err=%v", op, found, err)
			}
			if len(w.calls) != 0 {
				t.Fatal("owner rejection scheduled work")
			}
		})
	}
}

func TestPlanRunRegistrationCannotUpgradeInheritedEpoch(t *testing.T) {
	w := recoveryWorld(t)
	scope, err := w.sup.deps.Executions.Begin(t.Context(), execution.Key{TaskID: w.plan.TaskID, InstanceID: "original"})
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Finish(nil)
	if _, err := w.tasks.SetAside(w.plan.TaskID, task.StatePaused); err != nil {
		t.Fatal(err)
	}
	if _, err := w.tasks.Advance(w.plan.TaskID, task.StateRunning); err != nil {
		t.Fatal(err)
	}
	fresh, err := w.tasks.ExecutionToken(w.plan.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	w.plan.Execution = &fresh
	if _, err := w.sup.Execute(scope.Context(), w.plan); !errors.Is(err, task.ErrExecutionStopped) {
		t.Fatalf("stale scope registered a later epoch: %v", err)
	}
	if _, found, err := w.book.Operation(t.Context(), planRunID(w.plan.ID)); err != nil || found {
		t.Fatalf("stale scope left a run: found=%t err=%v", found, err)
	}
	if err := w.tasks.CheckExecution(fresh); err != nil {
		t.Fatal("rejection revoked the later epoch", err)
	}
}

func TestPlanRunPersistenceCannotReplaceOriginalOwner(t *testing.T) {
	w := recoveryWorld(t)
	rec, err := w.sup.opened(t.Context(), w.plan)
	if err != nil {
		t.Fatal(err)
	}
	before, _, _ := w.book.Operation(t.Context(), rec.ID)
	other, err := w.tasks.Create(task.Task{Channel: "other", ProjectID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	token, err := w.tasks.ExecutionToken(other.ID)
	if err != nil {
		t.Fatal(err)
	}
	rec.TaskID, rec.Execution = other.ID, &token
	if err := w.sup.saveRun(t.Context(), &rec, RunLanding); err == nil {
		t.Fatal("saveRun replaced the original owner")
	}
	after, _, err := w.book.Operation(t.Context(), rec.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("owner conflict changed run payload or phase", err)
	}
}

func TestPlanRunResumeCannotAdoptCallerEpoch(t *testing.T) {
	w := recoveryWorld(t)
	rec, err := w.sup.opened(t.Context(), w.plan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.tasks.SetAside(w.plan.TaskID, task.StatePaused); err != nil {
		t.Fatal(err)
	}
	if _, err := w.tasks.Advance(w.plan.TaskID, task.StateRunning); err != nil {
		t.Fatal(err)
	}
	fresh, err := w.tasks.ExecutionToken(w.plan.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	rec.Execution = &fresh
	if _, err := w.sup.Resume(t.Context(), rec); !errors.Is(err, task.ErrExecutionStopped) {
		t.Fatalf("Resume adopted caller authority instead of durable epoch: %v", err)
	}
	if len(w.calls) != 0 {
		t.Fatal("caller epoch reauthorized old plan work")
	}
	stored, found, err := w.sup.loadRun(t.Context(), rec.ID)
	if err != nil || !found || stored.Execution == nil || stored.Execution.Epoch == fresh.Epoch {
		t.Fatalf("old authority was overwritten: %+v %v", stored, err)
	}
	if err := w.tasks.CheckExecution(fresh); err != nil {
		t.Fatal("rejection revoked later permission", err)
	}
}
