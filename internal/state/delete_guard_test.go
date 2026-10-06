package state

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
)

func TestTaskDeletionCloseGuardUsesStrictFullDocument(t *testing.T) {
	for _, raw := range []string{
		`{"conversations":[],"owed_closes":[]}`,
		`{"conversations":{"chat":{"sessions":{"agent":{"unknown":true}}}}}`,
		`{"unknown":true}`,
		`{} {}`,
		`{"owed_closes":[`,
		`{"owed_closes":[{"node_id":17,"harness_id":"h","upstream_id":"session","task_id":"7","attempt_id":"attempt"}]}`,
		`null`,
	} {
		t.Run(raw, func(t *testing.T) {
			book := testLedger(t)
			if err := book.Document("state").Save([]byte(raw)); err != nil {
				t.Fatal(err)
			}
			check := func(tx ledger.Reader) error { return CheckTaskDeletionTx(tx, []string{"7"}) }
			if err := book.Read(t.Context(), func(tx *ledger.ReadTx) error { return check(tx) }); err == nil {
				t.Fatal("read guard accepted an unread state document")
			}
			if err := book.Update(t.Context(), func(tx *ledger.Tx) error { return check(tx) }); err == nil {
				t.Fatal("write guard accepted an unread state document")
			}
		})
	}
}

func TestTaskDeletionCloseGuardKeepsItsSnapshotAndTreeBoundary(t *testing.T) {
	book := testLedger(t)
	store, err := OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	check := func(ids ...string) error {
		return book.Read(t.Context(), func(tx *ledger.ReadTx) error { return CheckTaskDeletionTx(tx, ids) })
	}
	if err := check("7"); err != nil {
		t.Fatal("never-saved state should have no close obligations:", err)
	}
	owed := owedSession(t, store, "chat", "agent", "session")
	if err := store.ArchiveSessionOwingClose("chat", "agent", owed.OwedAt, owed); err != nil {
		t.Fatal(err)
	}
	if err := check("7"); !errors.Is(err, ErrCloseOwed) {
		t.Fatalf("owed close did not keep task authority: %v", err)
	}
	if err := check("other"); err != nil {
		t.Fatal("unrelated task blocked:", err)
	}
	if err := store.SettleOwedClose(owed); err != nil {
		t.Fatal(err)
	}
	if err := check("7"); err != nil {
		t.Fatal("refunded close still blocked:", err)
	}
	// A transaction must read its own newer document, not the store cache.
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
		if _, err := tx.Exec(`UPDATE bindings SET data=? WHERE kind='document' AND id='state'`, `{"owed_closes":[{"node_id":"node","harness_id":"h","upstream_id":"new","task_id":"7","attempt_id":"new"}]}`); err != nil {
			return err
		}
		if err := CheckTaskDeletionTx(tx, []string{"7"}); !errors.Is(err, ErrCloseOwed) {
			t.Fatalf("guard used stale cache instead of the transaction: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestTaskDeletionCloseGuardValidatesEvenUnrelatedObligations(t *testing.T) {
	for _, field := range []string{"task_id", "attempt_id", "harness_id", "upstream_id"} {
		t.Run(field, func(t *testing.T) {
			book := testLedger(t)
			owed := map[string]string{"task_id": "removed", "attempt_id": "attempt", "node_id": "", "harness_id": "h", "upstream_id": "session"}
			owed[field] = ""
			raw, err := json.Marshal(map[string]any{"owed_closes": []map[string]string{owed}})
			if err != nil {
				t.Fatal(err)
			}
			if err := book.Document("state").Save(raw); err != nil {
				t.Fatal(err)
			}
			if err := book.Read(t.Context(), func(tx *ledger.ReadTx) error { return CheckTaskDeletionTx(tx, []string{"other"}) }); err == nil {
				t.Fatal("unrelated malformed close was silently filtered out")
			}
		})
	}
}

func TestTaskDeletionCloseGuardHubLocalObligation(t *testing.T) {
	book := testLedger(t)
	store, err := OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	session := Session{ConversationID: "chat", AgentID: "agent", HarnessID: "h", UpstreamID: "hub-session"}
	if err := store.SaveSession(session); err != nil {
		t.Fatal(err)
	}
	owed := OwedClose{HarnessID: session.HarnessID, UpstreamID: session.UpstreamID, TaskID: "7", AttemptID: "hub-attempt"}
	if err := store.ArchiveSessionOwingClose("chat", "agent", "2026-10-06T00:00:00Z", owed); err != nil {
		t.Fatal(err)
	}
	check := func(ids []string, want error) {
		t.Helper()
		for _, write := range []bool{false, true} {
			var err error
			if write {
				err = book.Update(t.Context(), func(tx *ledger.Tx) error { return CheckTaskDeletionTx(tx, ids) })
			} else {
				err = book.Read(t.Context(), func(tx *ledger.ReadTx) error { return CheckTaskDeletionTx(tx, ids) })
			}
			if !errors.Is(err, want) {
				t.Errorf("hub-local guard ids=%v write=%v: %v, want %v", ids, write, err, want)
			}
		}
	}
	check([]string{"7"}, ErrCloseOwed)
	check([]string{"other"}, nil)
	if err := store.SettleOwedClose(owed); err != nil {
		t.Fatal(err)
	}
	check([]string{"7"}, nil)
	check([]string{"other"}, nil)
}
