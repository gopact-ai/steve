package execution

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/task"
)

func stopTestScope(t *testing.T, lifetime context.Context) (*Registry, *Scope, *task.Store) {
	t.Helper()
	tasks, err := task.Open(filepath.Join(t.TempDir(), "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	tracked, err := tasks.Create(task.Task{Channel: "console:test", Member: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	r := New(lifetime, tasks)
	s, err := r.Begin(t.Context(), Key{TaskID: tracked.ID, AttemptID: "attempt-1"})
	if err != nil {
		t.Fatal(err)
	}
	return r, s, tasks
}

func TestStopNativeHandlersPrecedeObserverCancellationAcrossNestedScopes(t *testing.T) {
	r, parent, tasks := stopTestScope(t, t.Context())
	child, err := r.Begin(parent.Context(), parent.key)
	if err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	err = RegisterStopHandler(child.Context(), "ns_original", func(ctx context.Context) error {
		calls.Add(1)
		// The registry must not hold its mutex across node RPCs.
		probe, err := r.Begin(t.Context(), Key{})
		if err != nil {
			return err
		}
		probe.Finish(nil)
		close(started)
		<-release
		if ctx.Err() != nil || child.Context().Err() != nil || parent.Context().Err() != nil {
			return errors.New("observer cancelled before native stop receipt")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ids, err := tasks.SetAside(parent.key.TaskID, task.StatePaused)
	if err != nil {
		t.Fatal(err)
	}
	waiting := r.Stop(ids, task.ErrExecutionStopped)
	<-started
	again := r.Stop(ids, task.ErrExecutionStopped)
	bounded, cancel := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel()
	if err := waiting.Wait(bounded); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stop did not wait for native receipt and owner cleanup: %v", err)
	}
	close(release)
	<-child.Context().Done()
	parent.Finish(nil)
	child.Finish(nil)
	if err := waiting.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := again.Wait(t.Context()); err != nil || calls.Load() != 1 {
		t.Fatalf("duplicate stop changed native execution: calls=%d err=%v", calls.Load(), err)
	}
}

func TestStopPreservesNativeFailureAndDriverCleanupFailure(t *testing.T) {
	r, s, tasks := stopTestScope(t, t.Context())
	nativeFailure := errors.New("node cannot confirm stopping")
	cleanupFailure := errors.New("result persistence failed")
	if err := RegisterStopHandler(s.Context(), "ns_original", func(context.Context) error { return nativeFailure }); err != nil {
		t.Fatal(err)
	}
	ids, _ := tasks.SetAside(s.key.TaskID, task.StateCancelled)
	waiting := r.Stop(ids, task.ErrExecutionStopped)
	<-s.Context().Done()
	s.Finish(cleanupFailure)
	err := waiting.Wait(t.Context())
	if !errors.Is(err, nativeFailure) || !errors.Is(err, cleanupFailure) {
		t.Fatalf("stop lost unresolved evidence: %v", err)
	}
	if err := r.Stop(ids, task.ErrExecutionStopped).Wait(t.Context()); !errors.Is(err, nativeFailure) {
		t.Fatalf("repeat stop forgot native failure: %v", err)
	}
}

func TestStopFailureCannotBeHiddenBySuccessfulDriverCleanup(t *testing.T) {
	r, s, tasks := stopTestScope(t, t.Context())
	nativeFailure := errors.New("node unavailable")
	if err := RegisterStopHandler(s.Context(), "ns_original", func(context.Context) error { return nativeFailure }); err != nil {
		t.Fatal(err)
	}
	ids, _ := tasks.SetAside(s.key.TaskID, task.StatePaused)
	waiting := r.Stop(ids, task.ErrExecutionStopped)
	s.Finish(nil)
	if err := waiting.Wait(t.Context()); !errors.Is(err, nativeFailure) {
		t.Fatalf("driver cleanup hid failed native stop: %v", err)
	}
	if err := r.Stop(ids, task.ErrExecutionStopped).Wait(t.Context()); !errors.Is(err, nativeFailure) {
		t.Fatalf("failed native stop was removed from registry: %v", err)
	}
}

func TestStopRegistrationAfterStopStillStopsLateOpenedSession(t *testing.T) {
	r, s, tasks := stopTestScope(t, t.Context())
	ids, _ := tasks.SetAside(s.key.TaskID, task.StateCancelled)
	waiting := r.Stop(ids, task.ErrExecutionStopped)
	<-s.Context().Done()
	var calls int
	err := RegisterStopHandler(s.Context(), "ns_late", func(ctx context.Context) error {
		calls++
		if ctx.Err() != nil {
			return errors.New("stop inherited cancelled observer context")
		}
		if _, ok := ctx.Deadline(); !ok {
			return errors.New("native stop has no deadline")
		}
		return nil
	})
	if !errors.Is(err, task.ErrExecutionStopped) || calls != 1 {
		t.Fatalf("late native session escaped task stop: calls=%d err=%v", calls, err)
	}
	s.Finish(nil)
	if err := waiting.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestShutdownAndLifetimeCancellationOnlyDetachNativeHandlers(t *testing.T) {
	for _, endLifetime := range []bool{false, true} {
		t.Run(map[bool]string{false: "shutdown", true: "lifetime"}[endLifetime], func(t *testing.T) {
			lifetime, cancel := context.WithCancel(t.Context())
			defer cancel()
			r, s, _ := stopTestScope(t, lifetime)
			var calls atomic.Int32
			if err := RegisterStopHandler(s.Context(), "ns_original", func(context.Context) error { calls.Add(1); return nil }); err != nil {
				t.Fatal(err)
			}
			if endLifetime {
				cancel()
			}
			done := make(chan error, 1)
			go func() { done <- r.Shutdown(t.Context()) }()
			<-s.Context().Done()
			s.Finish(nil)
			if err := <-done; err != nil || calls.Load() != 0 {
				t.Fatalf("detach stopped native execution: calls=%d err=%v", calls.Load(), err)
			}
		})
	}
}

func TestProbeContextDoesNotInstallNativeStopHandler(t *testing.T) {
	ctx := WithProbeKey(t.Context(), Key{TaskID: "task", AttemptID: "attempt"})
	if err := RegisterStopHandler(ctx, "ns_probe", func(context.Context) error { t.Error("probe stopped execution"); return nil }); err != nil {
		t.Fatalf("read-only probe was rejected: %v", err)
	}
	r, scope, tasks := stopTestScope(t, t.Context())
	ctx = WithProbeKey(scope.Context(), Key{TaskID: "other", AttemptID: "probe"})
	if Token(ctx) != nil {
		t.Fatal("read-only probe inherited running authorization")
	}
	if key, ok := KeyOf(ctx); !ok || key.TaskID != "other" || key.AttemptID != "probe" {
		t.Fatalf("probe inherited another execution's identity: %+v", key)
	}
	if err := RegisterStopHandler(ctx, "ns_probe", func(context.Context) error { t.Error("probe inherited a controlling scope"); return nil }); err != nil {
		t.Fatal(err)
	}
	ids, _ := tasks.SetAside(scope.key.TaskID, task.StatePaused)
	waiting := r.Stop(ids, task.ErrExecutionStopped)
	scope.Finish(nil)
	if err := waiting.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
}
