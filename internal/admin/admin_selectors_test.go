package admin

import (
	"reflect"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/turn/turntest"
)

// selectorAdminFixture has one agent and a default project for threads.
func selectorAdminFixture(t *testing.T) *Service {
	t.Helper()
	a, book := projectAdminFixture(t)
	catalog, err := agent.NewCatalog(map[string]agent.Config{"picker": {Harness: "codex", Default: true}})
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	a.Coordinator = turntest.New(t, func(o *turntest.Options) {
		o.Ledger, o.Catalog, o.Store, o.Timeout, o.Owner = book, catalog, store, time.Second, "owner"
		o.Projects, o.DefaultProject = a.Projects, "p"
	})
	return a
}

// The page may name a thread by its short name, as it does for its
// context and setup. Selector discovery and saved choices then belong to
// that console thread, not to a second conversation under the bare name.
func TestSelectorsBindTheConsoleThread(t *testing.T) {
	a := selectorAdminFixture(t)

	// No agent starts here, so discovery fails after the thread is bound.
	_, _ = a.Selectors(t.Context(), "t1", "picker")
	if _, bound, err := a.Projects.Binding(t.Context(), "t1"); err != nil || bound {
		t.Fatalf("selector discovery bound the bare name: bound=%v err=%v", bound, err)
	}
	if _, bound, err := a.Projects.Binding(t.Context(), "console:t1"); err != nil || !bound {
		t.Fatalf("selector discovery did not bind the console thread: bound=%v err=%v", bound, err)
	}
}

func TestPreferencesAreSavedForTheConsoleThread(t *testing.T) {
	a := selectorAdminFixture(t)

	if _, err := a.SetPreferences(t.Context(), "t1", "picker", map[string]string{"model": "m2"}); err != nil {
		t.Fatalf("set preferences: %v", err)
	}
	if got := a.Coordinator.Preferences("t1", "picker"); len(got) != 0 {
		t.Fatalf("choices were saved under the bare name: %v", got)
	}
	if got, want := a.Coordinator.Preferences("console:t1", "picker"), map[string]string{"model": "m2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("console thread choices = %v, want %v", got, want)
	}
}
