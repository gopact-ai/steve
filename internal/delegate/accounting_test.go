package delegate

import (
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/task"
)

func TestDelegateKnownExecutionNeverSettlesALaterUnboundRow(t *testing.T) {
	for _, fromRecord := range []bool{false, true} {
		t.Run(map[bool]string{false: "live-driver", true: "durable-receipt"}[fromRecord], func(t *testing.T) {
			w := newWorld(t)
			tracked, err := w.tasks.Create(task.Task{Goal: "later turn", Channel: "chat", Member: "builder"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.tasks.Begin(tracked.ID, "builder", "node-b", ""); err != nil {
				t.Fatal(err)
			}
			w.service.rememberAttempt(tracked.ID, "old-admitted-execution")
			if fromRecord {
				err = w.service.finishFromRecord(attempt.Record{Spec: attempt.Spec{ID: "old-admitted-execution", TaskID: tracked.ID, TurnID: "old-turn"}, EndedAt: time.Now(), Usage: &attempt.Usage{Input: 100, Output: 50, Reported: true}}, task.OutcomeCancelled)
			} else {
				err = w.service.finish(tracked.ID, task.OutcomeCancelled)
			}
			if err == nil {
				t.Fatal("known old execution consumed a newer unbound accounting row")
			}
			after, _ := w.tasks.Get(tracked.ID)
			if !after.Attempts[len(after.Attempts)-1].Open() || after.Budget.Tokens.Total != 0 {
				t.Fatalf("late old receipt changed current task row: %+v", after)
			}
		})
	}
}

func TestDelegateAdmissionFailureSettlesOnlyItsPreboundAccountingRow(t *testing.T) {
	w, db, _, _ := boundDelegatePersistenceFixture(t)
	if _, err := db.Exec(`CREATE TRIGGER reject_delegate_admission BEFORE INSERT ON operations WHEN NEW.kind = 'attempt' AND json_extract(NEW.data, '$.kind') = 'delegate' BEGIN SELECT RAISE(FAIL, 'admission storage unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	result, _ := w.service.Start(t.Context(), "chat", "codex", agentmcp.DelegateRequest{Goal: "fails before native open", Agent: "shipper"})
	if result.TaskID == "" {
		t.Fatal("test did not create its isolated child task")
	}
	for deadline := time.Now().Add(3 * time.Second); ; {
		tracked, ok := w.tasks.Get(result.TaskID)
		if ok && len(tracked.Attempts) == 1 && !tracked.Attempts[0].Open() {
			if tracked.Attempts[0].ExecutionID == "" || tracked.Attempts[0].TurnID != "delegate/"+tracked.ID || tracked.Budget.Turns != 1 || tracked.Budget.Tokens.Total != 0 {
				t.Fatalf("failed admission settled another row: %+v", tracked)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("failed admission did not settle its accounting: %+v", tracked)
		}
		time.Sleep(time.Millisecond)
	}
	w.sessions.mu.Lock()
	defer w.sessions.mu.Unlock()
	if len(w.sessions.opened) != 0 || len(w.sessions.prompts) != 0 {
		t.Fatal("admission failure launched native execution")
	}
}
