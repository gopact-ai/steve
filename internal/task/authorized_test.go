package task

import (
	"context"
	"errors"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
)

func authorizedWorld(t *testing.T) (*Store, *ledger.Ledger, Task, ExecutionToken) {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	store, err := OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	parent, err := store.Create(Task{Member: "parent", Channel: "chat"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Begin(parent.ID, "parent", "node-a", ""); err != nil {
		t.Fatal(err)
	}
	token, err := store.ExecutionToken(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	return store, book, parent, token
}
func TestSpawnAuthorizedRejectsRevokedExecutionAndGuardWithoutChangingTasks(t *testing.T) {
	for _, kind := range []string{"pause-resume", "grant-revoked", "grant-rolled-back"} {
		t.Run(kind, func(t *testing.T) {
			s, book, parent, token := authorizedWorld(t)
			denied := errors.New("original grant revoked")
			if kind == "pause-resume" {
				if _, err := s.SetAside(parent.ID, StatePaused); err != nil {
					t.Fatal(err)
				}
				if _, err := s.Advance(parent.ID, StateRunning); err != nil {
					t.Fatal(err)
				}
			}
			guard := func(tx *ledger.Tx) error {
				if kind == "grant-rolled-back" {
					if err := tx.PutBinding("test-grant", "touched", map[string]bool{"changed": true}); err != nil {
						return err
					}
				}
				return denied
			}
			if _, err := s.SpawnAuthorized(t.Context(), token, Task{Member: "child"}, guard); err == nil {
				t.Fatal("revoked grant spawned child")
			}
			if len(s.List("chat")) != 1 {
				t.Fatal("rejected command changed task cache")
			}
			stored, err := OpenLedger(book, "")
			if err != nil || len(stored.List("chat")) != 1 {
				t.Fatal("rejected command persisted a child")
			}
			if values, err := book.Bindings(t.Context(), "test-grant"); err != nil || len(values) != 0 {
				t.Fatalf("guard was not rolled back with child: %v %v", values, err)
			}
		})
	}
}
func TestSpawnAndConsumptionShareAuthorizedTaskTransaction(t *testing.T) {
	s, book, _, token := authorizedWorld(t)
	calls := 0
	guard := func(tx *ledger.Tx) error {
		calls++
		return tx.PutBinding("test-grant", "checked", map[string]int{"count": calls})
	}
	child, err := s.SpawnAuthorized(t.Context(), token, Task{Member: "child"}, guard)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("guard not called")
	}
	if err := s.SetDeliveryAuthorized(t.Context(), token, child.ID, DeliveryDelivered, func(*ledger.Tx) error { return errors.New("revoked before consumption") }); err == nil {
		t.Fatal("revoked grant consumed result")
	}
	stored, _ := s.Get(child.ID)
	if stored.Delivery != nil {
		t.Fatal("denied consumption marked delivery")
	}
	if err := s.SetDeliveryAuthorized(t.Context(), token, child.ID, DeliveryDelivered, guard); err != nil {
		t.Fatal(err)
	}
	loaded, err := OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	stored, _ = loaded.Get(child.ID)
	if stored.Delivery == nil || stored.Delivery.State != DeliveryDelivered {
		t.Fatal("authorized delivery not committed")
	}
}
func TestSpawnAuthorizedDoesNotOverwriteAnotherTaskStoreSnapshot(t *testing.T) {
	s, book, _, token := authorizedWorld(t)
	other, err := OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = other.Create(Task{Goal: "another source"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SpawnAuthorized(context.Background(), token, Task{Member: "child"}, nil); !errors.Is(err, ledger.ErrConflict) {
		t.Fatalf("stale task cache overwrote a concurrent task: %v", err)
	}
}
