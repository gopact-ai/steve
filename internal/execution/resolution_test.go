package execution

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/gopact-ai/steve/internal/task"
)

func TestResolveStoppedMatchesAttemptTaskAndOriginalEpoch(t *testing.T) {
	r, old, tasks := stopTestScope(t, t.Context())
	token := *old.Token()
	old.Finish(errors.New("old native stop unconfirmed"))
	other, err := tasks.Create(task.Task{Channel: "other", Member: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	otherScope, err := r.Begin(t.Context(), Key{TaskID: other.ID, InstanceID: "other-task", AttemptID: old.key.AttemptID})
	if err != nil {
		t.Fatal(err)
	}
	otherScope.Finish(errors.New("other task unresolved"))
	if _, err := tasks.SetAside(old.key.TaskID, task.StatePaused); err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Advance(old.key.TaskID, task.StateRunning); err != nil {
		t.Fatal(err)
	}
	newer, err := r.Begin(t.Context(), Key{TaskID: old.key.TaskID, InstanceID: "new-epoch", AttemptID: old.key.AttemptID})
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately reuse the attempt label: resolution must still compare
	// the task token, even after the new owner has itself become unresolved.
	newer.Finish(errors.New("new epoch unresolved"))
	r.ResolveStopped("another-attempt", token)
	if len(r.Active()) != 3 {
		t.Fatal("mismatched attempt identity resolved an owner")
	}
	r.ResolveStopped(old.key.AttemptID, task.ExecutionToken{TaskID: token.TaskID, Epoch: token.Epoch + 1})
	if len(r.Active()) != 3 {
		t.Fatal("mismatched execution epoch resolved an owner")
	}
	r.ResolveStopped(old.key.AttemptID, token)
	if got := r.Active(); !reflect.DeepEqual(got, []string{"new-epoch", "other-task"}) {
		t.Fatalf("old stop resolved another task/epoch or missed its owner: %v", got)
	}
	if _, err := r.SealIdle(); !errors.Is(err, ErrBusy) {
		t.Fatalf("old stop erased newer uncertainty: %v", err)
	}
	if err := tasks.CheckExecution(token); !errors.Is(err, task.ErrExecutionStopped) {
		t.Fatalf("resolution revived the revoked token: %v", err)
	}
}

func TestResolveStoppedDoesNotFinishOwnerOrNativeHandler(t *testing.T) {
	for _, phase := range []string{"owner", "handler"} {
		t.Run(phase, func(t *testing.T) {
			r, scope, tasks := stopTestScope(t, t.Context())
			token := *scope.Token()
			nativeErr := errors.New("original native observer failed")
			var release chan struct{}
			var waiting WaitSet
			if phase == "handler" {
				started := make(chan struct{})
				release = make(chan struct{})
				if err := RegisterStopHandler(scope.Context(), "ns_original", func(ctx context.Context) error {
					close(started)
					select {
					case <-release:
					case <-ctx.Done():
					}
					return nativeErr
				}); err != nil {
					t.Fatal(err)
				}
				ids, err := tasks.SetAside(scope.key.TaskID, task.StatePaused)
				if err != nil {
					t.Fatal(err)
				}
				waiting = r.Stop(ids, task.ErrExecutionStopped)
				<-started
				scope.Finish(nativeErr)
			} else if _, err := tasks.SetAside(scope.key.TaskID, task.StatePaused); err != nil {
				t.Fatal(err)
			}
			r.ResolveStopped(scope.key.AttemptID, token)
			if _, err := r.SealIdle(); !errors.Is(err, ErrBusy) {
				t.Errorf("durable proof discarded a still-running %s: %v", phase, err)
			}
			if phase == "handler" {
				close(release)
				if err := waiting.Wait(t.Context()); !errors.Is(err, nativeErr) {
					t.Fatalf("original stop lost its native failure: %v", err)
				}
			} else {
				scope.Finish(nativeErr)
			}
			r.ResolveStopped(scope.key.AttemptID, token)
			if got := r.Active(); len(got) != 0 {
				t.Fatalf("joined original owner did not converge: %v", got)
			}
		})
	}
}
