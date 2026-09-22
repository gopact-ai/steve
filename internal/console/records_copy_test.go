package console

import (
	"reflect"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/material"
	"github.com/gopact-ai/steve/internal/turn"
)

func TestConsoleUnchangedRepresentationsDoNotRewriteRetainedReceipts(t *testing.T) {
	s, book := consoleRecordFixture(t, 10000)
	// This is a real producer path: a harness with no selectors can provide
	// an explicitly empty options map. Its JSON omits the field.
	sample := s.resultReply(t.Context(), Exchange{Conversation: "console:records"}, newProcess(), turn.Result{
		Text: "answer", Injected: &turn.Injected{Agent: "dev", Harness: "test", Options: map[string]string{}},
	}, nil)
	for _, e := range s.exchanges["console:records"] {
		if e.Receipt != nil {
			e.Receipt.Injected = sample.Injected
		}
	}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`CREATE TABLE copy_write_audit(kind TEXT, bytes INTEGER)`,
		`CREATE TRIGGER copy_write_update AFTER UPDATE ON bindings BEGIN INSERT INTO copy_write_audit VALUES(new.kind,length(CAST(new.data AS BLOB))); END`,
	} {
		if _, err := book.DB().Exec(sql); err != nil {
			t.Fatal(err)
		}
	}
	title := "only metadata changes"
	if err := s.Update(t.Context(), "records", consoleapi.ConversationPatch{Title: &title}); err != nil {
		t.Fatal(err)
	}
	var rows, payload int
	if err := book.DB().QueryRow(`SELECT count(*),coalesce(sum(bytes),0) FROM copy_write_audit`).Scan(&rows, &payload); err != nil {
		t.Fatal(err)
	}
	t.Logf("10k receipts with empty options; title-only: rows=%d payload=%d", rows, payload)
	if rows != 2 || payload > 4096 {
		t.Fatalf("unchanged receipts serialized again: rows=%d payload=%d", rows, payload)
	}
}

func TestConsoleComparisonImagePreservesTypedValuesWithoutAliases(t *testing.T) {
	const conversation = "console:copy"
	now := time.Now() // Includes a monotonic reading absent from JSON.
	receipt := &consoleapi.Reply{
		ID: "receipt", Conversation: conversation, At: now,
		Injected: &consoleapi.Injected{Options: map[string]string{}, MCPServers: []string{}},
		Materials: []material.Frozen{{
			Ref: material.Ref{ID: "material", Selector: &material.Selector{Rect: &material.Rect{Width: 1}}},
			// Media bytes are deliberately excluded from persisted JSON.
			Media: &material.Media{Data: []byte("original")},
		}},
	}
	exchange := &queuedExchange{
		Exchange: Exchange{ID: "exchange", Conversation: conversation, EnqueuedAt: now, Quotes: []consoleapi.QuoteRef{}},
		Receipt:  receipt, QuoteAliases: map[string]string{"alias": "original"},
	}
	next := transcript{Exchanges: map[string][]*queuedExchange{conversation: {exchange}}}
	before := consoleRecords{values: map[consoleRecordKey]any{}}
	changes, err := consoleChanges(before, next)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range changes {
		before.values[change.key] = change.value
	}
	frozen := before.values[consoleRecordKey{consoleExchangeKind, exchange.ID}].(consoleExchangeRecord)
	if !reflect.DeepEqual(frozen.DurableExchange, durableExchange(exchange)) {
		t.Fatal("comparison image changed valid typed representations")
	}
	if unchanged, err := consoleChanges(before, next); err != nil || len(unchanged) != 0 {
		t.Fatalf("unchanged typed values look dirty: %d changes, %v", len(unchanged), err)
	}
	exchange.QuoteAliases["alias"] = "changed"
	receipt.Injected.Options["model"] = "changed"
	receipt.Materials[0].Ref.Selector.Rect.Width = 2
	receipt.Materials[0].Media.Data[0] = 'X'
	if frozen.QuoteAliases["alias"] != "original" || len(frozen.Receipt.Injected.Options) != 0 ||
		frozen.Receipt.Materials[0].Ref.Selector.Rect.Width != 1 ||
		string(frozen.Receipt.Materials[0].Media.Data) != "original" {
		t.Fatal("caller mutation reached the frozen comparison image")
	}
	changed, err := consoleChanges(before, next)
	if err != nil || len(changed) != 1 || changed[0].key.kind != consoleExchangeKind {
		t.Fatalf("real nested changes were lost: %+v %v", changed, err)
	}
}
