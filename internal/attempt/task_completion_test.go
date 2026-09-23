package attempt

import (
	"context"
	"errors"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

func TestReservationRechecksCompletionAfterLeaseAcquisition(t *testing.T) {
	service, _ := newService(t)
	tasks, err := task.OpenLedger(service.l)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := tasks.Create(task.Task{Channel: "chat"})
	if _, err := tasks.Advance(root.ID, task.StateRunning); err != nil {
		t.Fatal(err)
	}
	token, _ := tasks.ExecutionToken(root.ID)
	entered, release := make(chan struct{}), make(chan struct{})
	service.l.RegisterIssuer("remote", &fakeIssuer{onAcquire: func(context.Context) error {
		close(entered)
		<-release
		return nil
	}})
	admission := make(chan error, 1)
	go func() {
		_, err := service.Open(t.Context(), Spec{ID: "late", TaskID: root.ID, Execution: &token, Region: "remote", Kind: KindChat, Project: "p", Workspace: worktree("late", "p"), Scope: ScopePathSet})
		admission <- err
	}()
	<-entered
	_, completionErr := tasks.CompleteRoot(t.Context(), root.ID, root.Channel, CheckTaskCompletionTx)
	close(release)
	admissionErr := <-admission
	if completionErr != nil || !errors.Is(admissionErr, task.ErrExecutionStopped) {
		t.Fatalf("completion=%v reservation=%v", completionErr, admissionErr)
	}
	if records, err := service.ForTask(t.Context(), root.ID); err != nil || len(records) != 0 {
		t.Fatalf("late reservation committed: %+v %v", records, err)
	}
}

func TestTaskCompletionAndAttemptReservationHaveOneWinner(t *testing.T) {
	for range 30 {
		service, _ := newService(t)
		tasks, err := task.OpenLedger(service.l)
		if err != nil {
			t.Fatal(err)
		}
		root, err := tasks.Create(task.Task{Channel: "chat"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tasks.Advance(root.ID, task.StateRunning); err != nil {
			t.Fatal(err)
		}
		token, _ := tasks.ExecutionToken(root.ID)
		start := make(chan struct{})
		admission := make(chan error, 1)
		go func() {
			<-start
			_, err := service.Open(t.Context(), Spec{ID: "reserved", TaskID: root.ID, Execution: &token, Kind: KindChat, Project: "p", Workspace: worktree("reserved", "p"), Scope: ScopePathSet})
			admission <- err
		}()
		close(start)
		_, completionErr := tasks.CompleteRoot(t.Context(), root.ID, root.Channel, CheckTaskCompletionTx)
		admissionErr := <-admission
		if (completionErr == nil) == (admissionErr == nil) {
			t.Fatalf("need one winner: completion=%v reservation=%v", completionErr, admissionErr)
		}
		if completionErr == nil {
			if !errors.Is(admissionErr, task.ErrExecutionStopped) {
				t.Fatalf("unexpected refusal: %v", admissionErr)
			}
			if records, err := service.ForTask(t.Context(), root.ID); err != nil || len(records) != 0 {
				t.Fatalf("late reservation committed: %+v %v", records, err)
			}
		}
	}
}

func TestCompletionChecksAllAttemptStatesAndSettlement(t *testing.T) {
	for _, state := range []State{Leased, Prepared, Running, Snapshotted, Published, Durable, Verifying, BindReady, Bound, Failed, Expired, BindConflict, Superseded, "unknown"} {
		t.Run(string(state), func(t *testing.T) {
			service, _ := newService(t)
			settled := true
			record := Record{Spec: Spec{ID: "record", TaskID: "root"}, SessionSettled: &settled}
			if _, err := service.l.Begin(t.Context(), record.ID, kind, string(state), "test", record); err != nil {
				t.Fatal(err)
			}
			err := service.l.Update(t.Context(), func(tx *ledger.Tx) error { return CheckTaskCompletionTx(tx, map[string]bool{"root": true}) })
			allowed := state == Bound || state == Failed || state == Expired || state == BindConflict || state == Superseded
			if (err == nil) != allowed {
				t.Fatalf("%s: %v", state, err)
			}
		})
	}
}
