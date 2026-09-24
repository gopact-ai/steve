package task

import (
	"errors"
	"testing"
)

// A failed child is the case this exists for: the parent cannot finish while
// it sits there, and cancelling it would say the work was called off. The
// owner's decision settles it while the record keeps saying it failed.
func TestSettlingAFailedChildLetsItsParentFinishWithoutErasingTheFailure(t *testing.T) {
	for _, as := range []Settlement{SettlementHandled, SettlementIgnored} {
		t.Run(string(as), func(t *testing.T) {
			store, _ := newStore(t)
			root := idleCompletionRoot(t, store)
			child, _ := store.Spawn(root.ID, Task{Member: "child"})
			if _, err := store.Advance(child.ID, StateFailed); err != nil {
				t.Fatal(err)
			}
			if _, err := store.CompleteRoot(t.Context(), root.ID, root.Channel, admitAll); err == nil {
				t.Fatal("a failed child must hold its parent open until someone decides")
			}
			settled, err := store.Settle(child.ID, as)
			if err != nil {
				t.Fatalf("settle: %v", err)
			}
			if settled.State != StateFailed || settled.Settlement != as || settled.SettledAt.IsZero() {
				t.Fatalf("settling must record the decision beside the failure: %+v", settled)
			}
			if _, err := store.CompleteRoot(t.Context(), root.ID, root.Channel, admitAll); err != nil {
				t.Fatalf("complete after settling: %v", err)
			}
			reopened, _ := store.Get(child.ID)
			if reopened.State != StateFailed {
				t.Fatalf("the record must still say the work failed: %s", reopened.State)
			}
		})
	}
}

// Settling is a decision about work that already stopped. Anything still in
// play is paused or cancelled instead, and a late result cannot revive what
// its owner has closed.
func TestOnlyFailedWorkIsSettledAndSettlingRevokesItsAuthorization(t *testing.T) {
	store, _ := newStore(t)
	running := mustCreate(t, store, "still going", "chat")
	if _, err := store.Settle(running.ID, SettlementHandled); !errors.Is(err, ErrSettleState) {
		t.Fatalf("running work must not be settleable: %v", err)
	}
	if _, err := store.Advance(running.ID, StateFailed); err != nil {
		t.Fatal(err)
	}
	token, err := store.ExecutionToken(running.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Settle(running.ID, "elsewhere"); err == nil {
		t.Fatal("an unknown settlement must be refused")
	}
	if _, err := store.Settle(running.ID, SettlementIgnored); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if err := store.CheckExecution(token); !errors.Is(err, ErrExecutionStopped) {
		t.Fatalf("a settled task kept its execution authorization: %v", err)
	}
}

// Reopening is the way back: the decision goes, the failure stays, and the
// task can be retried as before.
func TestReopeningRestoresTheChoiceBetweenRetryAndCancel(t *testing.T) {
	store, _ := newStore(t)
	failed := mustCreate(t, store, "worth another look", "chat")
	if _, err := store.Advance(failed.ID, StateFailed); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Settle(failed.ID, SettlementHandled); err != nil {
		t.Fatal(err)
	}
	cleared, err := store.Settle(failed.ID, "")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if cleared.Settled() || !cleared.SettledAt.IsZero() || cleared.State != StateFailed {
		t.Fatalf("reopening must clear the decision and keep the failure: %+v", cleared)
	}
	if _, err := store.Advance(failed.ID, StateRunning); err != nil {
		t.Fatalf("a reopened task must still be retryable: %v", err)
	}
}
