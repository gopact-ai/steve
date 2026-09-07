package intent

import (
	"errors"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
)

func TestMigratedUnknownEffectRequiresReconciliationDespiteNewFingerprint(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	s := New(book)
	old, err := s.Claim(t.Context(), "old-task", "att", "channel_send", []byte(`{"conversation":"old","text":"progress"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Dispatched(t.Context(), old.ID); err != nil {
		t.Fatal(err)
	}
	facts, err := s.ExportTasks(t.Context(), map[string]bool{"old-task": true})
	if err != nil {
		t.Fatal(err)
	}
	if err := RemapTransfer(&facts, ledger.TransferIDs{Tasks: map[string]string{"old-task": "new-task"}}); err != nil {
		t.Fatal(err)
	}
	target, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if err := target.ImportFacts(t.Context(), facts, nil, nil); err != nil {
		t.Fatal(err)
	}
	imported := New(target)
	_, err = imported.Claim(t.Context(), "new-task", "new-att", "channel_send", []byte(`{"conversation":"new","text":"progress"}`))
	var blocked Blocked
	if !errors.As(err, &blocked) {
		t.Fatalf("namespace change replayed unknown action: %v", err)
	}
	if _, err := imported.Resolve(t.Context(), old.ID, "new", "operator reconciled"); err != nil {
		t.Fatal(err)
	}
	if _, err := imported.Claim(t.Context(), "new-task", "new-att", "channel_send", []byte(`{"conversation":"new","text":"progress"}`)); err != nil {
		t.Fatal("resolved action still blocked", err)
	}
}
