package admin

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/filedoc"
	"github.com/gopact-ai/steve/internal/nodewire"
)

// waitUntil polls for a condition the waiting restart reaches on its own.
func waitUntil(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if done() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestRestartWhenIdleWaitsForRunningWorkThenApplies(t *testing.T) {
	registry := execution.New(context.Background(), nil)
	running, err := registry.Begin(t.Context(), execution.Key{})
	if err != nil {
		t.Fatal(err)
	}
	var stopped atomic.Int32
	var sealed atomic.Bool
	s, err := NewServices(&Service{}, registry, func() (func(), error) { sealed.Store(true); return func() { sealed.Store(false) }, nil },
		func() { stopped.Add(1) }, &filedoc.Document{Path: filepath.Join(t.TempDir(), "restart.json")})
	if err != nil {
		t.Fatal(err)
	}
	op, err := s.Restart(t.Context(), "hub", consoleapi.RestartRequest{CommandID: "upgrade", Mode: consoleapi.RestartWhenIdle})
	if err != nil {
		t.Fatal(err)
	}
	if op.State != nodewire.RestartStateDraining || op.Mode != consoleapi.RestartWhenIdle {
		t.Fatalf("op=%+v", op)
	}
	waitUntil(t, "the running execution to be reported", func() bool {
		current, err := s.RestartStatus(t.Context(), "hub", "upgrade")
		return err == nil && current.WaitingOn == consoleapi.RestartWaitExecutions
	})
	if stopped.Load() != 0 || sealed.Load() {
		t.Fatalf("a waiting restart ended the service: stop=%d sealed=%v", stopped.Load(), sealed.Load())
	}
	// Repeating the request joins the one already waiting.
	again, err := s.Restart(t.Context(), "hub", consoleapi.RestartRequest{CommandID: "upgrade", Mode: consoleapi.RestartWhenIdle})
	if err != nil || again.State != nodewire.RestartStateDraining {
		t.Fatalf("again=%+v err=%v", again, err)
	}
	running.Finish(nil)
	waitUntil(t, "the restart to apply", func() bool { return stopped.Load() == 1 })
	applied, err := s.RestartStatus(t.Context(), "hub", "upgrade")
	if err != nil || applied.State != nodewire.RestartStateAccepted || !sealed.Load() {
		t.Fatalf("applied=%+v err=%v sealed=%v", applied, err, sealed.Load())
	}
	if !s.Requested() {
		t.Fatal("the applied restart was not requested of the launcher")
	}
}

func TestWithdrawnRestartLeavesTheServiceRunning(t *testing.T) {
	registry := execution.New(context.Background(), nil)
	running, err := registry.Begin(t.Context(), execution.Key{})
	if err != nil {
		t.Fatal(err)
	}
	defer running.Finish(nil)
	var stopped atomic.Int32
	s, err := NewServices(&Service{}, registry, func() (func(), error) { return func() {}, nil },
		func() { stopped.Add(1) }, &filedoc.Document{Path: filepath.Join(t.TempDir(), "restart.json")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Restart(t.Context(), "hub", consoleapi.RestartRequest{CommandID: "upgrade", Mode: consoleapi.RestartWhenIdle}); err != nil {
		t.Fatal(err)
	}
	withdrawn, err := s.Restart(t.Context(), "hub", consoleapi.RestartRequest{CommandID: "upgrade", Cancel: true})
	if err != nil || withdrawn.State != nodewire.RestartStateCancelled {
		t.Fatalf("withdrawn=%+v err=%v", withdrawn, err)
	}
	// The outcome stays readable, and the service is free to be asked again.
	recorded, err := s.RestartStatus(t.Context(), "hub", "upgrade")
	if err != nil || recorded.State != nodewire.RestartStateCancelled {
		t.Fatalf("recorded=%+v err=%v", recorded, err)
	}
	_, err = s.Restart(t.Context(), "hub", consoleapi.RestartRequest{CommandID: "second", Mode: consoleapi.RestartWhenIdle})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Restart(t.Context(), "hub", consoleapi.RestartRequest{CommandID: "second", Cancel: true}); err != nil {
		t.Fatal(err)
	}
	if stopped.Load() != 0 {
		t.Fatalf("a withdrawn restart stopped the service %d times", stopped.Load())
	}
}

// An immediate request replaces its own wait rather than colliding with it.
func TestImmediateRestartSupersedesItsOwnWait(t *testing.T) {
	var stopped atomic.Int32
	s, err := NewServices(&Service{}, execution.New(context.Background(), nil), func() (func(), error) { return func() {}, nil },
		func() { stopped.Add(1) }, &filedoc.Document{Path: filepath.Join(t.TempDir(), "restart.json")})
	if err != nil {
		t.Fatal(err)
	}
	// An idle service applies a waiting restart on its own, so this one is
	// held back by work that never finishes.
	registry := execution.New(context.Background(), nil)
	busy, err := registry.Begin(t.Context(), execution.Key{})
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Finish(nil)
	s.executions = registry
	if _, err := s.Restart(t.Context(), "hub", consoleapi.RestartRequest{CommandID: "upgrade", Mode: consoleapi.RestartWhenIdle}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the wait to report what it is waiting for", func() bool {
		current, err := s.RestartStatus(t.Context(), "hub", "upgrade")
		return err == nil && current.WaitingOn == consoleapi.RestartWaitExecutions
	})
	_, err = s.Restart(t.Context(), "hub", consoleapi.RestartRequest{CommandID: "upgrade"})
	var failure *consoleapi.ServiceError
	if !errors.As(err, &failure) || failure.Code != "busy" {
		t.Fatalf("an immediate restart of a busy service was not refused: %v", err)
	}
	if _, err := s.RestartStatus(t.Context(), "hub", "upgrade"); err == nil {
		t.Fatal("the superseded wait was left behind")
	}
}

// Replacing the program again while the first upgrade is still waiting has
// to take over the wait: refusing it would leave the service waiting to
// restart onto a build that is no longer installed.
func TestNewerWaitTakesOverAnOlderOne(t *testing.T) {
	registry := execution.New(context.Background(), nil)
	busy, err := registry.Begin(t.Context(), execution.Key{})
	if err != nil {
		t.Fatal(err)
	}
	var stopped atomic.Int32
	s, err := NewServices(&Service{}, registry, func() (func(), error) { return func() {}, nil },
		func() { stopped.Add(1) }, &filedoc.Document{Path: filepath.Join(t.TempDir(), "restart.json")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Restart(t.Context(), "hub", consoleapi.RestartRequest{CommandID: "upgrade-one", Mode: consoleapi.RestartWhenIdle}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the first upgrade to report what it is waiting for", func() bool {
		current, err := s.RestartStatus(t.Context(), "hub", "upgrade-one")
		return err == nil && current.WaitingOn == consoleapi.RestartWaitExecutions
	})
	second, err := s.Restart(t.Context(), "hub", consoleapi.RestartRequest{CommandID: "upgrade-two", Mode: consoleapi.RestartWhenIdle})
	if err != nil || second.State != nodewire.RestartStateDraining {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	// The first command keeps an honest outcome instead of disappearing.
	first, err := s.RestartStatus(t.Context(), "hub", "upgrade-one")
	if err != nil || first.State != nodewire.RestartStateCancelled {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	busy.Finish(nil)
	waitUntil(t, "the surviving upgrade to apply", func() bool { return stopped.Load() == 1 })
	applied, err := s.RestartStatus(t.Context(), "hub", "upgrade-two")
	if err != nil || applied.State != nodewire.RestartStateAccepted {
		t.Fatalf("applied=%+v err=%v", applied, err)
	}
}

// Being told only that something is active is what sends a person looking
// through every conversation. The refusal names the work instead.
func TestBusyRestartNamesTheExecutionHoldingIt(t *testing.T) {
	registry := execution.New(context.Background(), nil)
	running, err := registry.Begin(t.Context(), execution.Key{InstanceID: "turn-7"})
	if err != nil {
		t.Fatal(err)
	}
	defer running.Finish(nil)
	s, err := NewServices(&Service{}, registry, func() (func(), error) { return func() {}, nil },
		func() {}, &filedoc.Document{Path: filepath.Join(t.TempDir(), "restart.json")})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Restart(t.Context(), "hub", consoleapi.RestartRequest{CommandID: "now"})
	var failure *consoleapi.ServiceError
	if !errors.As(err, &failure) || failure.Reason != consoleapi.RestartWaitExecutions {
		t.Fatalf("refusal=%v", err)
	}
	if !strings.Contains(failure.Message, "turn-7") {
		t.Fatalf("the refusal did not name the execution: %q", failure.Message)
	}
}
