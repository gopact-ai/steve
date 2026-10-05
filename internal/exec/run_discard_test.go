package exec

import (
	"errors"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
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
