package turntest_test

import (
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn"
	"github.com/gopact-ai/steve/internal/turn/turntest"
)

func TestNewBuildsACompleteCoordinator(t *testing.T) {
	if turntest.New(t) == nil {
		t.Fatal("turntest.New returned nil")
	}
}

func TestDepsKeepWhatATestSetsAndOpenTheRestOnItsLedger(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = book.Close() })
	tasks, err := task.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	deps := turntest.Deps(t, func(o *turntest.Options) {
		o.Ledger = book
		o.Tasks, o.Node = tasks, "hub"
	})
	if deps.Tasks != tasks || deps.Node != "hub" {
		t.Fatal("Deps replaced what the test set")
	}
	// A default store opened on the test's ledger sees its writes.
	if err := deps.Store.SetActiveAgent("c", "a"); err != nil {
		t.Fatal(err)
	}
	if got := turntest.Deps(t, func(o *turntest.Options) { o.Ledger = book }).Store.Conversation("c").ActiveAgent; got != "a" {
		t.Fatalf("a second Deps on the same ledger reads active agent %q, want a", got)
	}
}

// A package outside turn can name the runtime a coordinator is built with.
var _ turn.Runtime = turntest.NoRuntime{}
