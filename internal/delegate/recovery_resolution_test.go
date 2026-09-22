package delegate

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/task"
)

// The native transport is the existing retained-session fixture; admission,
// process-stop proof, accounting and registry reconciliation use real owners.
func TestStoppedChildResolvesJoinedObserverAfterAccounting(t *testing.T) {
	for _, rejectAccounting := range []bool{false, true} {
		name := "successful-accounting"
		if rejectAccounting {
			name = "rejected-accounting-then-retry"
		}
		t.Run(name, func(t *testing.T) {
			w, sessions, _, child := detachedDelegateFixture(t)
			service := recoveredDelegateService(t, w, sessions)
			sessions.mu.Lock()
			sessions.inspectErr = errors.New("temporary node outage")
			sessions.mu.Unlock()
			if err := service.RecoverRetained(t.Context()); err != nil {
				t.Fatal(err)
			}
			waitDelegateRecoveryOwner(t, service, child.TaskID)
			if len(service.executions.Active()) != 1 {
				t.Fatal("failed inspection must retain the unresolved original observer")
			}
			records, err := w.attempts.ForTask(t.Context(), child.TaskID)
			if err != nil || len(records) != 1 {
				t.Fatalf("original attempt: %+v %v", records, err)
			}
			book := w.book
			if rejectAccounting {
				if _, err := book.DB().Exec(`CREATE TRIGGER reject_child_accounting BEFORE UPDATE ON bindings
					WHEN NEW.kind='task-attempt'
					BEGIN SELECT RAISE(ABORT, 'accounting unavailable'); END`); err != nil {
					t.Fatal(err)
				}
			}
			sessions.mu.Lock()
			sessions.inspectErr, sessions.processStopped = nil, true
			sessions.mu.Unlock()
			if err := service.RecoverRetained(t.Context()); err != nil {
				t.Fatal(err)
			}
			waitDelegateRecoveryOwner(t, service, child.TaskID)
			if rejectAccounting {
				tracked, _ := w.tasks.Get(child.TaskID)
				if !tracked.Attempts[0].Open() || len(service.executions.Active()) == 0 {
					t.Fatal("failed accounting erased unresolved execution ownership")
				}
				if _, err := book.DB().Exec(`DROP TRIGGER reject_child_accounting`); err != nil {
					t.Fatal(err)
				}
				if err := service.RecoverRetained(t.Context()); err != nil {
					t.Fatal(err)
				}
				waitDelegateRecoveryOwner(t, service, child.TaskID)
			}
			stored := awaitDelegateResult(t, w.tasks, child.TaskID)
			record, err := w.attempts.Get(t.Context(), records[0].ID)
			if err != nil || record.Unsettled || record.SessionSettled == nil || !*record.SessionSettled ||
				record.StopEvidence != "process-stop/"+record.ID || stored.Attempts[0].Open() {
				t.Fatalf("missing original physical stop/accounting: record=%+v task=%+v err=%v", record, stored, err)
			}
			for range 3 {
				if err := service.RecoverRetained(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			if active := service.executions.Active(); len(active) != 0 {
				t.Errorf("durably settled, joined child still has unresolved observers: %v", active)
			}
			if release, err := service.executions.SealIdle(); err != nil {
				t.Errorf("maintenance blocked after process stop and accounting: %v", err)
			} else {
				release()
			}
			if err := service.executions.WhileTaskIdle(child.TaskID, func() error { return nil }); err != nil {
				t.Errorf("settled child still blocks completion: %v", err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if err := service.executions.Stop([]string{child.TaskID}, task.ErrExecutionStopped).Wait(ctx); err != nil {
				t.Errorf("later stop reports obsolete uncertainty: %v", err)
			}
			sessions.mu.Lock()
			defer sessions.mu.Unlock()
			if sessions.prompts != 1 || sessions.resumes != 0 {
				t.Fatalf("physical stop replayed native work: prompts=%d resumes=%d", sessions.prompts, sessions.resumes)
			}
		})
	}
}

func waitDelegateRecoveryOwner(t *testing.T, service *Service, taskID string) {
	t.Helper()
	service.mu.Lock()
	entry := service.pending[taskID]
	service.mu.Unlock()
	// detachChild removes the pending entry only after Scope.Finish.
	if entry == nil {
		return
	}
	select {
	case <-entry.done:
	case <-time.After(3 * time.Second):
		t.Fatal("recovery owner did not finish")
	}
	if entry.scope != nil {
		select {
		case <-entry.scope.Done():
		case <-time.After(3 * time.Second):
			t.Fatal("recovery scope did not finish")
		}
	}
}

func TestRecoveredChildResolutionRequiresOriginalSettledAccounting(t *testing.T) {
	w, _, _, child := boundDelegatePersistenceFixture(t)
	service := recoveredDelegateService(t, w, w.sessions)
	records, err := w.attempts.ForTask(t.Context(), child.ID)
	if err != nil || len(records) != 1 {
		t.Fatalf("original attempt: %+v %v", records, err)
	}
	record := records[0]
	scope, err := service.executions.BeginAccepted(t.Context(), execution.Key{
		TaskID: child.ID, AttemptID: record.ID, InstanceID: "original",
	}, record.Execution)
	if err != nil {
		t.Fatal(err)
	}
	scope.Finish(errors.New("original observer lost"))
	if err := service.finishFromRecord(record, task.OutcomeOK); err != nil {
		t.Fatal(err)
	}
	if err := w.tasks.SetResult(child.ID, task.Result{Attempt: record.ID}); err != nil {
		t.Fatal(err)
	}
	tracked, _ := w.tasks.Get(child.ID)
	for _, scenario := range []string{"open", "turn", "epoch", "unknown-usage", "missing-usage", "counter", "model", "result"} {
		t.Run(scenario, func(t *testing.T) {
			mutated, _ := w.tasks.Get(child.ID)
			row := &mutated.Attempts[0]
			switch scenario {
			case "open":
				row.EndedAt = time.Time{}
			case "turn":
				row.TurnID = "other"
			case "epoch":
				row.ExecutionEpoch++
			case "unknown-usage":
				known := false
				row.UsageKnown = &known
			case "missing-usage":
				row.UsageKnown = nil
			case "counter":
				row.Tokens.Input++
			case "model":
				row.Model = "other"
			case "result":
				mutated.Result.Attempt = "other"
			}
			service.resolveRecovered(record, mutated)
			if len(service.executions.Active()) != 1 {
				t.Fatal("inconsistent accounting resolved the original observer")
			}
		})
	}
	service.resolveRecovered(record, tracked)
	if len(service.executions.Active()) != 0 {
		t.Fatal("matching durable accounting did not resolve the joined observer")
	}
}

func TestRetainedSweepRetriesResolutionAfterOriginalNativeHandlerJoins(t *testing.T) {
	w, _, _, child := boundDelegatePersistenceFixture(t)
	service := recoveredDelegateService(t, w, w.sessions)
	records, err := w.attempts.ForTask(t.Context(), child.ID)
	if err != nil || len(records) != 1 {
		t.Fatalf("original attempt: %+v %v", records, err)
	}
	record := records[0]
	scope, err := service.executions.BeginAccepted(t.Context(), execution.Key{
		TaskID: child.ID, AttemptID: record.ID, InstanceID: "original",
	}, record.Execution)
	if err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	nativeErr := errors.New("old handler did not observe its final receipt")
	if err := execution.RegisterStopHandler(scope.Context(), "original", func(ctx context.Context) error {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nativeErr
	}); err != nil {
		t.Fatal(err)
	}
	ids, err := w.tasks.SetAside(child.ID, task.StatePaused)
	if err != nil {
		t.Fatal(err)
	}
	waiting := service.executions.Stop(ids, task.ErrExecutionStopped)
	<-started
	scope.Finish(nativeErr)
	if err := service.finishFromRecord(record, task.OutcomeOK); err != nil {
		t.Fatal(err)
	}
	if err := w.tasks.SetResult(child.ID, task.Result{Attempt: record.ID}); err != nil {
		t.Fatal(err)
	}
	if err := service.RecoverRetained(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(service.executions.Active()) != 1 {
		t.Fatal("durable result/accounting discarded a still-running native handler")
	}
	close(release)
	if err := waiting.Wait(t.Context()); !errors.Is(err, nativeErr) {
		t.Fatalf("original wait lost its handler outcome: %v", err)
	}
	if err := service.RecoverRetained(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(service.executions.Active()) != 0 {
		t.Fatal("paused child skipped resolution after the original handler joined")
	}
	if err := w.tasks.CheckExecution(*record.Execution); !errors.Is(err, task.ErrExecutionStopped) {
		t.Fatal("cleanup restored the old execution token")
	}
}
