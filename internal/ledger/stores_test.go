package ledger_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
)

// A deployment that wrote JSON files before the ledger existed must come up
// with everything it had, once, and never read those files as authority
// again.
func TestStoresImportLegacyFilesOnceAndPersistInLedger(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	tasksPath := filepath.Join(dir, "tasks.json")
	plansPath := filepath.Join(dir, "plans.json")

	// The old world: three files.
	oldState, err := state.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := oldState.SetActiveAgent("oc_1", "claude"); err != nil {
		t.Fatal(err)
	}
	oldTasks, err := task.Open(tasksPath)
	if err != nil {
		t.Fatal(err)
	}
	created, err := oldTasks.Create(task.Task{Channel: "oc_1", Member: "claude", Goal: "legacy goal"})
	if err != nil {
		t.Fatal(err)
	}
	oldPlans, err := plan.Open(plansPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := oldPlans.Create(plan.Plan{TaskID: created.ID, Steps: []plan.Step{{ID: "s1", Goal: "one", Agent: "claude", Verify: &plan.Verify{Kind: plan.VerifyNone, Why: "smoke"}}}}); err != nil {
		t.Fatal(err)
	}

	// The new world opens the ledger and imports.
	book, err := ledger.Open(dir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	newState, err := state.OpenLedger(book, statePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := newState.Conversation("oc_1").ActiveAgent; got != "claude" {
		t.Fatalf("active agent after import = %q", got)
	}
	newTasks, err := task.OpenLedger(book, tasksPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := newTasks.Get(created.ID); !ok || got.Goal != "legacy goal" {
		t.Fatalf("task after import = %+v ok=%v", got, ok)
	}
	newPlans, err := plan.OpenLedger(book, plansPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := newPlans.ForTask(created.ID); !ok || len(got.Steps) != 1 {
		t.Fatalf("plan after import = %+v ok=%v", got, ok)
	}
	for _, p := range []string{statePath, tasksPath, plansPath} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s still exists as authority", p)
		}
		if _, err := os.Stat(p + ".migrated"); err != nil {
			t.Fatalf("%s was not retired: %v", p, err)
		}
	}

	// Writes land in the ledger and survive a reopen; the retired file is
	// not consulted even if it reappears.
	if _, err := newTasks.Create(task.Task{Channel: "oc_1", Member: "claude", Goal: "new goal"}); err != nil {
		t.Fatal(err)
	}
	book.Close()
	if err := os.WriteFile(tasksPath, []byte(`{"next_id":1,"tasks":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	book2, err := ledger.Open(dir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book2.Close()
	again, err := task.OpenLedger(book2, tasksPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := again.List("oc_1"); len(got) != 2 {
		t.Fatalf("tasks after reopen = %d, want 2", len(got))
	}
	if _, err := os.Stat(tasksPath); err != nil {
		t.Fatal("a stray legacy file was consumed after the import already happened")
	}
}
