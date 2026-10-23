package turn

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/state"
)

func handleExchange(c *Coordinator, t *testing.T, input, exchange, target string) (Result, error) {
	return c.Handle(t.Context(), Request{
		Source:    Source{ConversationID: "chat"},
		Input:     input,
		Admission: Admission{ExchangeID: exchange, StopTarget: target},
	})
}

// A stop bound to an exchange reaches only the turn running that exchange:
// once that turn has ended it stops nothing, neither the turn running now
// nor one that begins next.
func TestStopTargetDoesNotReachAnotherTurn(t *testing.T) {
	t.Parallel()
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Default: true}})
	store, _ := state.OpenLedger(testLedger(t))
	runner := &fakeRunner{started: make(chan struct{}), done: make(chan struct{})}
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": runner}}
	coordinator := newCoordinator(t, catalog, store, capability.NewAssembler(nil), manager, time.Minute)
	turnDone := make(chan error, 1)
	go func() {
		_, err := handleExchange(coordinator, t, "second task", "e-second", "")
		turnDone <- err
	}()
	select {
	case <-runner.started:
	case <-time.After(waitDeadline):
		t.Fatal("turn did not start")
	}
	// The stop was meant for e-first, which ended before the stop ran.
	result, err := handleExchange(coordinator, t, "/cancel", "e-stop", "e-first")
	if err != nil || result.Text != i18n.New(i18n.LocaleZH).T(i18n.NoRunningTurn) {
		t.Fatalf("stale stop = %#v, %v", result, err)
	}
	if runner.cancels.Load() != 0 {
		t.Fatal("a stop for an ended turn cancelled the turn running now")
	}
	close(runner.done)
	if err := <-turnDone; err != nil {
		t.Fatalf("the running turn was disturbed: %v", err)
	}
	// Nothing was armed for the next turn either.
	if _, err := handleExchange(coordinator, t, "third task", "e-third", ""); err != nil {
		t.Fatalf("a stale stop stopped the next turn: %v", err)
	}
}

// A stop bound to the exchange that is running stops it.
func TestStopTargetStopsItsTurn(t *testing.T) {
	t.Parallel()
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Default: true}})
	store, _ := state.OpenLedger(testLedger(t))
	runner := &fakeRunner{started: make(chan struct{}), done: make(chan struct{})}
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": runner}}
	coordinator := newCoordinator(t, catalog, store, capability.NewAssembler(nil), manager, time.Minute)
	turnDone := make(chan error, 1)
	go func() {
		_, err := handleExchange(coordinator, t, "long task", "e-long", "")
		turnDone <- err
	}()
	select {
	case <-runner.started:
	case <-time.After(waitDeadline):
		t.Fatal("turn did not start")
	}
	result, err := handleExchange(coordinator, t, "/cancel", "e-stop", "e-long")
	if err != nil || result.Text != i18n.New(i18n.LocaleZH).T(i18n.CancelRequested, "codex") {
		t.Fatalf("stop = %#v, %v", result, err)
	}
	select {
	case err := <-turnDone:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(waitDeadline):
		t.Fatal("the targeted turn did not stop")
	}
	if runner.cancels.Load() == 0 {
		t.Fatal("the targeted turn was not cancelled")
	}
}
