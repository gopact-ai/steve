package task

import (
	"errors"
	"slices"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
)

func TestDeleteChannelRechecksTheCompleteTreeInItsWrite(t *testing.T) {
	s, _ := newStore(t)
	parent := mustCreate(t, s, "root", "console:cleanup")
	child, err := s.Spawn(parent.ID, Task{Goal: "child", Channel: "delegate:cleanup"})
	if err != nil {
		t.Fatal(err)
	}
	guard := func(reader ledger.Reader, ids []string) error {
		if !slices.Contains(ids, parent.ID) || !slices.Contains(ids, child.ID) {
			t.Fatal("deletion guard lost its task tree")
		}
		var pending int
		if err := reader.QueryRow(`SELECT count(*) FROM bindings WHERE kind='cleanup-test'`).Scan(&pending); err != nil {
			return err
		}
		if pending != 0 {
			return ErrRetirementPending
		}
		return nil
	}
	if err := s.ChannelIdle(t.Context(), parent.Channel, guard); err != nil {
		t.Fatal(err)
	}
	if err := s.book.Update(t.Context(), func(tx *ledger.Tx) error { return tx.PutBinding("cleanup-test", "later", true) }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteChannel(t.Context(), parent.Channel, guard); !errors.Is(err, ErrRetirementPending) {
		t.Fatalf("delete after preflight=%v; missing final cleanup guard", err)
	}
	for _, id := range []string{parent.ID, child.ID} {
		if _, found := s.Get(id); !found {
			t.Fatal("refused deletion removed cached task")
		}
	}
	reopened, err := OpenLedger(s.book)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{parent.ID, child.ID} {
		if _, found := reopened.Get(id); !found {
			t.Fatal("refused deletion removed durable task")
		}
	}
	if err := s.book.Update(t.Context(), func(tx *ledger.Tx) error {
		_, err := tx.Exec(`DELETE FROM bindings WHERE kind='cleanup-test'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteChannel(t.Context(), parent.Channel, guard); err != nil {
		t.Fatalf("settled history could not be removed: %v", err)
	}
}

func TestDeleteChannelKeepsTheCacheWhenTheWriteIsRefused(t *testing.T) {
	s, _ := newStore(t)
	tracked := mustCreate(t, s, "keep", "console:refused")
	if err := s.book.Update(t.Context(), func(tx *ledger.Tx) error {
		_, err := tx.Exec(`CREATE TRIGGER refuse_task_delete BEFORE DELETE ON bindings WHEN OLD.kind='task' BEGIN SELECT RAISE(ABORT,'delete refused'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteChannel(t.Context(), tracked.Channel, func(ledger.Reader, []string) error { return nil }); err == nil {
		t.Fatal("refused delete accepted")
	}
	if _, found := s.Get(tracked.ID); !found {
		t.Fatal("refused delete changed cache")
	}
}
