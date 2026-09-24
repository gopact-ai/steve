package task

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestBudgetSourceAppliesOnlyToNewTasksAndCanRestoreUnlimited(t *testing.T) {
	store, err := OpenLedger(testBook(t))
	if err != nil {
		t.Fatal(err)
	}
	type defaults struct {
		turns   int
		elapsed time.Duration
	}
	var current atomic.Pointer[defaults]
	current.Store(&defaults{turns: 7, elapsed: time.Hour})
	store.BudgetSource = func() (int, time.Duration) {
		p := current.Load()
		return p.turns, p.elapsed
	}
	first := mustCreate(t, store, "retained task", "a")
	current.Store(&defaults{})
	second := mustCreate(t, store, "new task", "b")
	if second.Budget.MaxTurns != 0 || second.Budget.MaxElapsed != 0 {
		t.Fatalf("zero failed to restore unlimited defaults: %+v", second.Budget)
	}
	retained, _ := store.Get(first.ID)
	if retained.Budget.MaxTurns != 7 || retained.Budget.MaxElapsed != time.Hour {
		t.Fatalf("hot settings rewrote retained budget: %+v", retained.Budget)
	}
}
