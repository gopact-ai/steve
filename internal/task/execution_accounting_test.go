package task

import (
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
)

func TestDelayedStopUsageSettlesItsOriginalRowAfterTaskResumes(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	s, err := OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	tracked, err := s.Create(Task{Goal: "original", Channel: "console:main", Member: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Begin(tracked.ID, "worker", "node-a", ""); err != nil {
		t.Fatal(err)
	}
	original, _ := s.ExecutionToken(tracked.ID)
	if err := s.BindAttempt(original, "old-execution", "old-turn"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetAside(tracked.ID, StatePaused); err != nil {
		t.Fatal(err)
	}
	if err := s.SettleAttempt(tracked.ID, "old-execution", "old-turn", time.Now(), OutcomeCancelled, RecoveryUsage{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(tracked.ID, StateRunning); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Begin(tracked.ID, "worker", "node-b", ""); err != nil {
		t.Fatal(err)
	}
	current, _ := s.ExecutionToken(tracked.ID)
	if err := s.BindAttempt(current, "new-execution", "new-turn"); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Get(tracked.ID)
	if err := s.SettleAttempt(tracked.ID, "old-execution", "new-turn", time.Now(), OutcomeCancelled, RecoveryUsage{Reported: true, Tokens: Tokens{Input: 999}}); err == nil {
		t.Fatal("wrong turn accepted old usage")
	}
	usage := RecoveryUsage{Tokens: Tokens{Input: 100, Output: 50}, Model: "original-model", Reported: true}
	for range 2 {
		if err := s.SettleAttempt(tracked.ID, "old-execution", "old-turn", time.Now(), OutcomeCancelled, usage); err != nil {
			t.Fatal(err)
		}
	}
	after, _ := s.Get(tracked.ID)
	if after.State != StateRunning || after.ExecutionEpoch != current.Epoch || after.Budget.Turns != 2 || after.Budget.Tokens.Total != 150 || after.Budget.Elapsed != before.Budget.Elapsed || len(after.Attempts) != 2 || !after.Attempts[1].Open() || after.Attempts[1].ExecutionID != "new-execution" || after.Attempts[1].Tokens.Total != 0 {
		t.Fatalf("late receipt changed current execution or double-charged original: %+v", after)
	}
	if after.Attempts[0].UsageKnown == nil || !*after.Attempts[0].UsageKnown || after.Attempts[0].Model != "original-model" {
		t.Fatal("late reported usage not retained")
	}
}
