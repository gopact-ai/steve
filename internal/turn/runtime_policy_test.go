package turn

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/state"
)

func TestLiveTimeoutAppliesToNextTurnWithoutInterruptingCurrent(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Default: true}})
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	runner := &fakeRunner{started: make(chan struct{}), done: make(chan struct{})}
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": runner}}
	c := newCoordinator(t, catalog, store, capability.NewAssembler(nil), manager, time.Hour)
	var duration atomic.Int64
	duration.Store(int64(time.Minute))
	c.TimeoutSource = func() time.Duration { return time.Duration(duration.Load()) }
	done := make(chan error, 1)
	go func() { _, err := handle(c, t.Context(), "first"); done <- err }()
	select {
	case <-runner.started:
	case <-time.After(waitDeadline):
		t.Fatal("first turn did not start")
	}
	duration.Store(int64(time.Nanosecond))
	select {
	case err := <-done:
		t.Fatalf("policy change interrupted the current turn: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(runner.done)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := handle(c, t.Context(), "next"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("next turn did not use new silence limit: %v", err)
	}
	if runner.aborts.Load() != 0 || runner.cancels.Load() != 0 {
		t.Fatal("saving policy stopped the native process")
	}
}

func TestLiveDefaultLocaleAndExplicitRequestLocale(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"worker": {Harness: "mock", Default: true}})
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	c := New(catalog, store, nil, nil, time.Minute)
	var english atomic.Bool
	c.SetCatalog(i18n.Dynamic(func() i18n.Locale {
		if english.Load() {
			return i18n.LocaleEN
		}
		return i18n.LocaleZH
	}))
	for _, want := range []i18n.Locale{i18n.LocaleZH, i18n.LocaleEN} {
		english.Store(want == i18n.LocaleEN)
		got, err := c.Handle(t.Context(), Request{ConversationID: string(want), Input: "/use worker"})
		if err != nil || got.Text != i18n.New(want).T(i18n.Switched, "worker") {
			t.Fatalf("default language not live: %q %v", got.Text, err)
		}
	}
	got, err := c.Handle(t.Context(), Request{ConversationID: "explicit", Input: "/use worker", Locale: "zh"})
	if err != nil || got.Text != i18n.New(i18n.LocaleZH).T(i18n.Switched, "worker") {
		t.Fatal("default language overrode request preference")
	}
}
