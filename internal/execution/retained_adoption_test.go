package execution

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

func retainedSQLiteRegistry(t *testing.T) (*Registry, *task.Store, task.Task) {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = book.Close() })
	tasks, err := task.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	tracked, err := tasks.Create(task.Task{Channel: "test", Member: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	return New(t.Context(), tasks), tasks, tracked
}

func TestAdoptRetainedRequiresExactTaskAndToken(t *testing.T) {
	for _, mode := range []string{"other-task", "new-epoch"} {
		t.Run(mode, func(t *testing.T) {
			r, tasks, tracked := retainedSQLiteRegistry(t)
			prior, err := r.Begin(t.Context(), Key{TaskID: tracked.ID, AttemptID: "same-label", InstanceID: "prior"})
			if err != nil {
				t.Fatal(err)
			}
			prior.Finish(errors.New("prior native writer not confirmed"))
			target := tracked.ID
			if mode == "other-task" {
				other, err := tasks.Create(task.Task{Channel: "other", Member: "agent"})
				if err != nil {
					t.Fatal(err)
				}
				target = other.ID
			} else {
				if _, err := tasks.SetAside(target, task.StatePaused); err != nil {
					t.Fatal(err)
				}
				if _, err := tasks.Advance(target, task.StateRunning); err != nil {
					t.Fatal(err)
				}
			}
			current, err := r.Begin(t.Context(), Key{TaskID: target, AttemptID: "same-label", InstanceID: "current"})
			if err != nil {
				t.Fatal(err)
			}
			current.AdoptRetained()
			current.Finish(nil)
			if active := r.Active(); len(active) != 1 || active[0] != "prior" {
				t.Errorf("adoption removed another task/token's uncertainty: %v", active)
			}
			if release, err := r.SealIdle(); !errors.Is(err, ErrBusy) {
				if release != nil {
					release()
				}
				t.Errorf("maintenance permitted without original stop/accounting: %v", err)
			}
		})
	}
}

func TestAdoptRetainedCannotDropUnjoinedNativeHandler(t *testing.T) {
	r, tasks, tracked := retainedSQLiteRegistry(t)
	prior, err := r.Begin(t.Context(), Key{TaskID: tracked.ID, AttemptID: "original", InstanceID: "prior"})
	if err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	nativeErr := errors.New("native stop has no receipt")
	if err := RegisterStopHandler(prior.Context(), "ns_original", func(ctx context.Context) error {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nativeErr
	}); err != nil {
		t.Fatal(err)
	}
	prior.Finish(errors.New("old observer detached"))
	// A new observer is admitted while the original token is still valid;
	// task stop races its later AdoptRetained, as separate owner calls allow.
	current, err := r.BeginAccepted(t.Context(), Key{TaskID: tracked.ID, AttemptID: "original", InstanceID: "current"}, prior.Token())
	if err != nil {
		t.Fatal(err)
	}
	ids, err := tasks.SetAside(tracked.ID, task.StatePaused)
	if err != nil {
		t.Fatal(err)
	}
	waiting := r.Stop(ids, task.ErrExecutionStopped)
	<-started
	current.AdoptRetained()
	r.mu.Lock()
	_, exists := r.entries[prior]
	r.mu.Unlock()
	if !exists {
		t.Error("AdoptRetained removed old scope while its native handler was still executing")
	}
	current.Finish(nil)
	close(release)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := waiting.Wait(ctx); !errors.Is(err, nativeErr) {
		t.Fatalf("original Wait lost handler error: %v", err)
	}
	if release, err := r.SealIdle(); !errors.Is(err, ErrBusy) {
		if release != nil {
			release()
		}
		t.Errorf("handler failed, no durable proof was ever supplied, but Registry is idle: %v", err)
	}
}

func TestAdoptRetainedWithoutTaskAuthorityCannotEraseObserver(t *testing.T) {
	r := New(t.Context(), nil)
	prior, err := r.Begin(t.Context(), Key{AttemptID: "original"})
	if err != nil {
		t.Fatal(err)
	}
	prior.Finish(errors.New("native execution remains unknown"))
	current, err := r.Begin(t.Context(), Key{AttemptID: "original"})
	if err != nil {
		t.Fatal(err)
	}
	current.AdoptRetained()
	current.Finish(nil)
	if len(r.Active()) != 1 {
		t.Fatal("an unbound observer erased unknown execution ownership")
	}
}
