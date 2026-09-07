package task

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
)

func TestParallelReservedExecutionsHaveIndependentAccountingAndSharedBudget(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	s, err := OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	parent, err := s.Create(Task{Goal: "plan", Member: "parent", Budget: Budget{MaxTurns: 3}})
	if err != nil {
		t.Fatal(err)
	}
	token, _ := s.ExecutionToken(parent.ID)
	start := time.Now().UTC().Add(-time.Second)
	var wg sync.WaitGroup
	errors := make(chan error, 8)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.ReserveAttempt(token, fmt.Sprintf("step-%d", i), fmt.Sprintf("turn-%d", i), "worker", "node-a", start)
			errors <- err
		}()
	}
	wg.Wait()
	close(errors)
	accepted := 0
	for err := range errors {
		if err == nil {
			accepted++
		}
	}
	tracked, _ := s.Get(parent.ID)
	if accepted != 3 || len(tracked.Attempts) != 3 || tracked.Budget.Turns != 3 || tracked.Member != "parent" {
		t.Fatalf("parallel admission overspent or changed task owner: accepted=%d task=%+v", accepted, tracked)
	}
	first, second := tracked.Attempts[0], tracked.Attempts[1]
	for range 2 {
		if _, err := s.ReserveAttempt(token, first.ExecutionID, first.TurnID, first.Member, first.Node, first.StartedAt); err != nil {
			t.Fatal(err)
		}
		if err := s.SettleAttempt(parent.ID, first.ExecutionID, first.TurnID, start.Add(time.Second), OutcomeOK, RecoveryUsage{Tokens: Tokens{Input: 7, Output: 3}, Reported: true}); err != nil {
			t.Fatal(err)
		}
	}
	tracked, _ = s.Get(parent.ID)
	if tracked.Budget.Turns != 3 || tracked.Budget.Tokens.Total != 10 || tracked.Budget.Elapsed != time.Second || !tracked.Attempts[1].Open() || tracked.Attempts[1].ExecutionID != second.ExecutionID {
		t.Fatalf("one execution consumed another row or double charged: %+v", tracked)
	}
	if _, err := s.SetAside(parent.ID, StatePaused); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(parent.ID, StateRunning); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReserveAttempt(token, "late", "late", "worker", "node-a", start); err == nil {
		t.Fatal("old authorization admitted a new execution")
	}
	if err := s.SettleAttempt(parent.ID, second.ExecutionID, second.TurnID, start.Add(time.Second), OutcomeCancelled, RecoveryUsage{Tokens: Tokens{Input: 5}, Reported: true}); err != nil {
		t.Fatal(err)
	}
	tracked, _ = s.Get(parent.ID)
	if tracked.Budget.Tokens.Total != 15 || !tracked.Attempts[2].Open() || tracked.State != StateRunning {
		t.Fatalf("late receipt changed other execution: %+v", tracked)
	}
}

func TestReservedExecutionBudgetAndIdentityRollbackTogether(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	s, err := OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	tracked, err := s.Create(Task{Goal: "plan"})
	if err != nil {
		t.Fatal(err)
	}
	token, _ := s.ExecutionToken(tracked.ID)
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
		_, err := tx.Exec(`CREATE TRIGGER reject_accounting BEFORE UPDATE ON bindings WHEN NEW.kind='document' AND NEW.id='tasks' BEGIN SELECT RAISE(FAIL,'unavailable'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReserveAttempt(token, "step", "turn", "worker", "node", time.Now()); err == nil {
		t.Fatal("failed write admitted execution")
	}
	after, _ := s.Get(tracked.ID)
	if after.Budget.Turns != 0 || len(after.Attempts) != 0 || after.State != tracked.State {
		t.Fatalf("failed write escaped rollback: %+v", after)
	}
}

func TestIndependentAccountingDoesNotHideOrCloseOtherOpenExecutions(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	s, err := OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	tracked, err := s.Create(Task{Goal: "conversation", Channel: "chat", Member: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Begin(tracked.ID, "parent", "local", "session"); err != nil {
		t.Fatal(err)
	}
	token, _ := s.ExecutionToken(tracked.ID)
	for _, id := range []string{"parallel-a", "parallel-b"} {
		if _, err := s.ReserveAttempt(token, id, id, "worker", "node", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SettleAttempt(tracked.ID, "parallel-b", "parallel-b", time.Now(), OutcomeOK, RecoveryUsage{}); err != nil {
		t.Fatal(err)
	}
	if running, ok := s.Running("chat", "parent"); !ok || running.ID != tracked.ID {
		t.Fatal("later completed independent row hid an active execution")
	}
	if len(s.Interrupted()) != 1 {
		t.Fatal("open independent/primary execution disappeared from recovery")
	}
	if err := s.BindAttempt(token, "original-chat", "chat-turn"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Begin(tracked.ID, "parent", "local", "session-2"); err == nil {
		t.Fatal("independent completion admitted an overlapping chat turn")
	}
	if _, err := s.FinishAs(tracked.ID, OutcomeOK, Tokens{Input: 7, Total: 7}, 0, "chat-model"); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Get(tracked.ID)
	if after.Attempts[0].Open() || after.Attempts[0].ExecutionID != "original-chat" || !after.Attempts[1].Open() || after.Attempts[1].Tokens.Total != 0 || !after.HasOpenExecution() {
		t.Fatalf("finishing primary turn changed parallel accounting: %+v", after)
	}
	if _, ok := s.Running("chat", "parent"); !ok {
		t.Fatal("remaining parallel execution stopped being visible")
	}
	if _, err := s.Begin(tracked.ID, "parent", "local", "session-2"); err != nil {
		t.Fatal(err)
	}
	if err := s.BindAttempt(token, "next-chat", "next-turn"); err != nil {
		t.Fatal(err)
	}
	if err := s.SettleAttempt(tracked.ID, "parallel-a", "parallel-a", time.Now(), OutcomeOK, RecoveryUsage{Tokens: Tokens{Input: 3}, Reported: true}); err != nil {
		t.Fatal(err)
	}
	after, _ = s.Get(tracked.ID)
	if !after.Attempts[3].Open() || after.Attempts[3].ExecutionID != "next-chat" || after.Budget.Tokens.Total != 10 {
		t.Fatalf("parallel completion changed next primary turn: %+v", after)
	}
}
