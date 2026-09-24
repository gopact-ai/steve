package turntest_test

import (
	"path/filepath"
	"testing"

	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/memory"
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

// The default memory keeps global memory in the home the coordinator reads,
// whether the test chose that home or left it to the default.
func TestDefaultMemoryLivesInTheCoordinatorsHome(t *testing.T) {
	chosen := t.TempDir()
	for name, deps := range map[string]turn.Deps{
		"chosen":  turntest.Deps(t, func(o *turntest.Options) { o.Home = home.Dir{Path: chosen} }),
		"default": turntest.Deps(t),
	} {
		want := filepath.Join(deps.Home.(home.Dir).Path, home.FileMemory)
		if got := deps.Memory.Where(memory.Global); got != want {
			t.Errorf("%s home: global memory at %q, want %q", name, got, want)
		}
	}
}

func TestNewWiresTheCoordinator(t *testing.T) {
	c := turntest.New(t)
	defer func() {
		if recover() == nil {
			t.Fatal("a coordinator turntest.New built was not wired")
		}
	}()
	c.Wire(turntest.Callbacks(turn.Callbacks{}))
}

func TestUnwiredLeavesWireToTheTest(t *testing.T) {
	// Wire panics on a coordinator that is already wired.
	turntest.Unwired(t).Wire(turntest.Callbacks(turn.Callbacks{}))
}

// IdleCoordinator runs no turn, so a test that sends it one fails at once
// instead of taking an error reply for the answer.
func TestIdleCoordinatorPanicsWhenAskedToRunATurn(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("IdleCoordinator.Handle returned instead of panicking")
		}
	}()
	_, _ = turntest.IdleCoordinator{}.Handle(t.Context(), turn.Request{})
}
