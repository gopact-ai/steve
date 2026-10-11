package events

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/durable"
	"github.com/gopact-ai/steve/internal/ledger"
)

func TestJournalRetainsColdEnvelopesAndDoesNotConsumeOrReturnPayloads(t *testing.T) {
	book, err := ledger.Open(filepath.Join(t.TempDir(), "book"), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	service := &Service{Book: book}
	ids := []string{}
	at := time.Now().UTC().Add(-10 * 24 * time.Hour)
	for i := range 34 {
		var event Event
		err := book.Update(t.Context(), func(tx *ledger.Tx) error {
			var err error
			event, err = AppendTx(tx, Event{Source: "server:journal-test", Key: fmt.Sprint(i), Type: "test.notice", Subject: "Original subject", At: at, Payload: json.RawMessage(`{"private":"` + strings.Repeat("never copy this payload into a polling page ", 30) + `"}`)})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, event.ID)
	}
	sort.Strings(ids)
	// One entry stays in the outbox. A routed, compressed entry must still
	// retain its original source/time without falsely claiming consumption.
	for _, id := range ids[1:] {
		if err := service.Route(t.Context(), id); err != nil {
			t.Fatal(err)
		}
	}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
		saved, err := durable.ArchiveTx(tx, EventKind, ids[1])
		if err == nil && saved <= 0 {
			return fmt.Errorf("fixture did not compress an original event")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	first, err := service.Journal(t.Context(), "")
	if err != nil || len(first.Items) != 32 || first.Next == "" {
		t.Fatalf("bounded journal first page: items=%d next=%q err=%v", len(first.Items), first.Next, err)
	}
	second, err := service.Journal(t.Context(), first.Next)
	if err != nil || len(second.Items) != 2 || second.Next != "" {
		t.Fatalf("bounded journal continuation: items=%d next=%q err=%v", len(second.Items), second.Next, err)
	}
	seen := map[string]bool{}
	for _, item := range append(first.Items, second.Items...) {
		if seen[item.ID] || item.Source != "server:journal-test" || item.Subject != "Original subject" || !item.At.Equal(at) || item.Routed != (item.ID != ids[0]) {
			t.Fatalf("journal changed or duplicated an original fact: %+v", item)
		}
		seen[item.ID] = true
	}
	raw, err := json.Marshal(first)
	if err != nil || strings.Contains(string(raw), "payload") || strings.Contains(string(raw), "never copy") {
		t.Fatalf("polling page leaked payload: %v", err)
	}
	if _, exists, err := durable.Read[string](t.Context(), book, OutboxKind, ids[0]); err != nil || !exists {
		t.Fatalf("observation consumed the pending event: exists=%v err=%v", exists, err)
	}
}
