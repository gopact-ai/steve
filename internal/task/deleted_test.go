package task

import (
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
)

func deletedIn(t *testing.T, book *ledger.Ledger, id string) bool {
	t.Helper()
	var deleted bool
	if err := book.Read(t.Context(), func(tx *ledger.ReadTx) error {
		var err error
		deleted, err = DeletedTx(tx, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return deleted
}

// A deleted task is one the store issued and removed whole. A task that
// still exists, one never issued, and one whose header is gone while its
// accounting remains are not deletions.
func TestDeletedTxProvesOnlyAWholeDeletion(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	s, err := OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	gone, _ := s.Create(Task{Goal: "gone", Channel: "chat-gone", Member: "worker"})
	kept, _ := s.Create(Task{Goal: "kept", Channel: "chat-kept", Member: "worker"})
	partial, _ := s.Create(Task{Goal: "partial", Channel: "chat-partial", Member: "worker"})
	for _, id := range []string{gone.ID, partial.ID} {
		if _, err := s.Begin(id, "worker", "", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Finish(id, OutcomeOK, Tokens{}, 0); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DeleteChannel("chat-gone"); err != nil {
		t.Fatal(err)
	}
	if _, err := book.DB().Exec(`DELETE FROM bindings WHERE kind='task' AND id=?`, partial.ID); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]bool{gone.ID: true, kept.ID: false, partial.ID: false, "99": false, "": false, "01": false} {
		if got := deletedIn(t, book, id); got != want {
			t.Errorf("DeletedTx(%q) = %v; want %v", id, got, want)
		}
	}
}
