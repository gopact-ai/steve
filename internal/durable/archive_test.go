package durable

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
)

func TestColdRecordsPreserveExactFactsAcrossAllTypedReadsAndRestart(t *testing.T) {
	dir := t.TempDir()
	book, err := ledger.Open(dir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	type fact struct {
		ID      string          `json:"id"`
		State   string          `json:"state"`
		Payload json.RawMessage `json:"payload"`
		Output  string          `json:"output"`
	}
	want := fact{ID: "original", State: "done", Payload: json.RawMessage(`{"message_id":9007199254740993,"text":"你好"}`), Output: strings.Repeat("original execution output\n", 400)}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
		if err := tx.PutBinding("fact", want.ID, want); err != nil {
			return err
		}
		return tx.PutBinding("fact-index", want.ID, want.ID)
	}); err != nil {
		t.Fatal(err)
	}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
		saved, err := ArchiveTx(tx, "fact", want.ID)
		if saved <= 0 {
			t.Fatal("terminal payload was not compressed")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	assertReads := func() {
		t.Helper()
		got, found, err := Read[fact](t.Context(), book, "fact", want.ID)
		if err != nil || !found || !reflect.DeepEqual(got, want) {
			t.Fatalf("cold read changed original fact: %+v %v", got, err)
		}
		page, err := PageOf[fact](t.Context(), book, "fact", "")
		if err != nil || len(page.Items) != 1 || !reflect.DeepEqual(page.Items[0], want) {
			t.Fatalf("cold list changed original fact: %+v %v", page, err)
		}
		indexed, err := PageByIndex[fact](t.Context(), book, "fact-index", "fact", "")
		if err != nil || len(indexed.Items) != 1 || !reflect.DeepEqual(indexed.Items[0], want) {
			t.Fatalf("cold indexed page changed original fact: %+v %v", indexed, err)
		}
	}
	assertReads()
	if err := book.Close(); err != nil {
		t.Fatal(err)
	}
	book, err = ledger.Open(dir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	assertReads()
	// A copied pointer must not make another immutable ID return this fact.
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
		var raw []byte
		if err := tx.QueryRow(`SELECT data FROM bindings WHERE kind=? AND id=?`, "fact", want.ID).Scan(&raw); err != nil {
			return err
		}
		return tx.PutBinding("fact", "different", json.RawMessage(raw))
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Read[fact](t.Context(), book, "fact", "different"); err == nil {
		t.Fatal("cross-record cold pointer returned a different fact")
	}
}
