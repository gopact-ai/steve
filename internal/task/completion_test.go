package task

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
)

func idleCompletionRoot(t *testing.T, store *Store) Task {
	t.Helper()
	root := mustCreate(t, store, "accepted work", "chat")
	if _, err := store.Begin(root.ID, "parent", "node", "native-session"); err != nil {
		t.Fatal(err)
	}
	root, err := store.Finish(root.ID, OutcomeOK, Tokens{Total: 17}, 2)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestCompleteIdleRootPreservesHistoryAndRevokesEpoch(t *testing.T) {
	store, _ := newStore(t)
	root := idleCompletionRoot(t, store)
	token, _ := store.ExecutionToken(root.ID)
	completed, err := store.CompleteRoot(t.Context(), root.ID, root.Channel, nil)
	if err != nil || completed.State != StateDone {
		t.Fatalf("complete: %+v %v", completed, err)
	}
	if !reflect.DeepEqual(completed.Attempts, root.Attempts) || completed.Budget != root.Budget || completed.ExecutionEpoch != root.ExecutionEpoch+1 {
		t.Fatal("completion lost accounting/context or retained execution authorization")
	}
	if _, ok := store.Active(root.Channel, root.Member, ""); ok {
		t.Fatal("completed root still claims conversation")
	}
	if err := store.CheckExecution(token); !errors.Is(err, ErrExecutionStopped) {
		t.Fatalf("old token: %v", err)
	}
	if err := store.CheckExecution(ExecutionToken{TaskID: completed.ID, Epoch: completed.ExecutionEpoch}); !errors.Is(err, ErrExecutionStopped) {
		t.Fatalf("explicit closure accepted fresh authorization: %v", err)
	}
	if _, err := store.BeginContinuation(root.ID, root.Channel, root.Member, "node"); !errors.Is(err, ErrContinuationUnavailable) {
		t.Fatalf("old continuation: %v", err)
	}
	if _, err := store.Advance(root.ID, StateRunning); err == nil {
		t.Fatal("completed root resumed")
	}
	for range 2 {
		repeated, err := store.CompleteRoot(t.Context(), root.ID, root.Channel, nil)
		if err != nil || !reflect.DeepEqual(completed, repeated) {
			t.Fatalf("unstable completion retry: %+v %v", repeated, err)
		}
	}
	if _, err := store.CompleteRoot(t.Context(), root.ID, "foreign", nil); err == nil {
		t.Fatal("terminal retry bypassed ownership")
	}
	for _, interrupted := range store.Interrupted() {
		if interrupted.ID == root.ID {
			t.Fatal("completed root selected for restart")
		}
	}
}

func TestCompleteRootRequiresClosedDeliveredChildren(t *testing.T) {
	for _, state := range []string{"running", "failed", "paused", "missing-result", "missing-delivery", DeliveryPending, DeliveryQueued, DeliveryUncertain, DeliverySuppressed, DeliveryDelivered} {
		t.Run(state, func(t *testing.T) {
			store, _ := newStore(t)
			root := idleCompletionRoot(t, store)
			child, err := store.Spawn(root.ID, Task{Member: "child", Origin: "delegate:" + root.ID})
			if err != nil {
				t.Fatal(err)
			}
			token, _ := store.ExecutionToken(child.ID)
			to := StateDone
			if state == "running" || state == "failed" || state == "paused" {
				to = State(state)
			}
			if _, err := store.Advance(child.ID, to); err != nil {
				t.Fatal(err)
			}
			if state != "missing-result" {
				if err := store.SetResult(child.ID, Result{Outcome: OutcomeOK, Answer: "accepted"}); err != nil {
					t.Fatal(err)
				}
			}
			if state != "missing-delivery" {
				if err := store.SetDelivery(child.ID, state); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.CompleteRoot(t.Context(), child.ID, child.Channel, nil); !errors.Is(err, ErrCompleteRoot) {
				t.Fatalf("child completion: %v", err)
			}
			_, err = store.CompleteRoot(t.Context(), root.ID, root.Channel, nil)
			if (err == nil) != (state == DeliveryDelivered) {
				t.Fatalf("completion with child %s: %v", state, err)
			}
			if err == nil && !errors.Is(store.CheckExecution(token), ErrExecutionStopped) {
				t.Fatal("descendant authorization survived root completion")
			}
		})
	}
}

func TestCompletionRefusesNonChatAndUnsettledAttempts(t *testing.T) {
	for _, kind := range []string{"plan", "schedule:1", "prepared", "draft", "paused", "failed", "blocked", "cancelled", "old-open", "reserved", "grandchild"} {
		t.Run(kind, func(t *testing.T) {
			store, _ := newStore(t)
			root := idleCompletionRoot(t, store)
			switch kind {
			case "plan", "schedule:1":
				store.data.Tasks[root.ID].Origin = kind
			case "prepared":
				store.data.Tasks[root.ID].PreparedPlan = &PreparedPlan{}
			case "old-open":
				store.data.Tasks[root.ID].Attempts = append(root.Attempts, Attempt{StartedAt: time.Now(), ExecutionEpoch: root.ExecutionEpoch + 5})
			case "reserved":
				token, _ := store.ExecutionToken(root.ID)
				if _, err := store.ReserveAttempt(token, "reserved", "turn", "worker", "node", time.Now()); err != nil {
					t.Fatal(err)
				}
			case "grandchild":
				child, _ := store.Spawn(root.ID, Task{Member: "child"})
				_, _ = store.Spawn(child.ID, Task{Member: "grandchild"})
				_, _ = store.Advance(child.ID, StateDone)
				_ = store.SetResult(child.ID, Result{Outcome: OutcomeOK})
				_ = store.SetDelivery(child.ID, DeliveryDelivered)
			default:
				store.data.Tasks[root.ID].State = State(kind)
			}
			if _, err := store.CompleteRoot(t.Context(), root.ID, root.Channel, nil); err == nil {
				t.Fatalf("completed %s", kind)
			}
		})
	}
}

func TestCompletionSerializesWithExecutionAndDelegationAdmission(t *testing.T) {
	for _, kind := range []string{"begin", "continuation", "reserve", "spawn"} {
		t.Run(kind, func(t *testing.T) {
			for range 20 {
				store, _ := newStore(t)
				root := idleCompletionRoot(t, store)
				token, _ := store.ExecutionToken(root.ID)
				start := make(chan struct{})
				admitted := make(chan error, 1)
				go func() {
					<-start
					var err error
					switch kind {
					case "begin":
						_, err = store.Begin(root.ID, root.Member, "node", "")
					case "continuation":
						_, err = store.BeginContinuation(root.ID, root.Channel, root.Member, "node")
					case "reserve":
						_, err = store.ReserveAttempt(token, "reserved", "turn", "worker", "node", time.Now())
					case "spawn":
						_, err = store.Spawn(root.ID, Task{Member: "child"})
					}
					admitted <- err
				}()
				close(start)
				_, completionErr := store.CompleteRoot(t.Context(), root.ID, root.Channel, nil)
				admissionErr := <-admitted
				if (completionErr == nil) == (admissionErr == nil) {
					t.Fatalf("need exactly one winner: complete=%v admission=%v", completionErr, admissionErr)
				}
			}
		})
	}
}

func TestCompletionGuardAndStateCommitTogether(t *testing.T) {
	store, book, root, token := authorizedWorld(t)
	if _, err := store.Finish(root.ID, OutcomeOK, Tokens{}, 0); err != nil {
		t.Fatal(err)
	}
	before, _ := store.Get(root.ID)
	denied := errors.New("pending result")
	_, err := store.CompleteRoot(t.Context(), root.ID, root.Channel, func(tx *ledger.Tx, ids map[string]bool) error {
		if !ids[root.ID] {
			t.Fatal("guard lost root")
		}
		if err := tx.PutBinding("completion-test", "receipt", true); err != nil {
			return err
		}
		return denied
	})
	if !errors.Is(err, denied) {
		t.Fatalf("guard: %v", err)
	}
	after, _ := store.Get(root.ID)
	if !reflect.DeepEqual(before, after) || store.CheckExecution(token) != nil {
		t.Fatal("rejected completion changed task or epoch")
	}
	if rows, err := book.Bindings(t.Context(), "completion-test"); err != nil || len(rows) != 0 {
		t.Fatalf("guard did not roll back: %v %v", rows, err)
	}
	if _, err := store.CompleteRoot(t.Context(), root.ID, root.Channel, nil); err == nil {
		t.Fatal("ledger completion without durable guard")
	}
}

func TestAutomaticDoneIsNotAnExplicitCompletionReceipt(t *testing.T) {
	store, _ := newStore(t)
	root := idleCompletionRoot(t, store)
	token, _ := store.ExecutionToken(root.ID)
	if _, err := store.Advance(root.ID, StateDone); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteRoot(t.Context(), root.ID, root.Channel, nil); !errors.Is(err, ErrCompleteState) {
		t.Fatalf("automatic done mistaken for user completion: %v", err)
	}
	if err := store.CheckExecution(token); err != nil {
		t.Fatalf("normal done no longer permits accepted results: %v", err)
	}
}
