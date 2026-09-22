package console

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/readmodel"
)

func consoleRecordFixture(t testing.TB, count int) (*Service, *ledger.Ledger) {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	s := New(&echo{}, "owner", nil)
	if err := s.PersistLedger(book); err != nil {
		t.Fatal(err)
	}
	const conversation = "console:records"
	s.replies[conversation] = []consoleapi.Reply{{ID: "reply", Conversation: conversation, Kind: "reply", Text: "unchanged evidence"}}
	s.meta[conversation] = Meta{Title: "before"}
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("e-%d", i)
		s.exchanges[conversation] = append(s.exchanges[conversation], &queuedExchange{
			Exchange: Exchange{ID: id, Conversation: conversation, Key: "client:" + id, State: consoleapi.ExchangeDone, Input: strings.Repeat("history ", 128)},
			Receipt:  &consoleapi.Reply{ID: "r-" + id, Conversation: conversation, ExchangeID: id, Kind: "reply", Text: strings.Repeat("evidence ", 128)},
		})
	}
	s.exchanges[conversation] = append(s.exchanges[conversation], &queuedExchange{Exchange: Exchange{ID: "queued", Conversation: conversation, State: consoleapi.ExchangeQueued, Input: "before"}})
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	return s, book
}

func TestConsoleTenThousandHistorySmallWritesHaveBoundedPayload(t *testing.T) {
	s, book := consoleRecordFixture(t, 10000)
	for _, sql := range []string{
		`CREATE TABLE console_write_audit(kind TEXT, id TEXT, bytes INTEGER)`,
		`CREATE TRIGGER console_write_insert AFTER INSERT ON bindings BEGIN INSERT INTO console_write_audit VALUES(new.kind,new.id,length(CAST(new.data AS BLOB))); END`,
		`CREATE TRIGGER console_write_update AFTER UPDATE ON bindings BEGIN INSERT INTO console_write_audit VALUES(new.kind,new.id,length(CAST(new.data AS BLOB))); END`,
	} {
		if _, err := book.DB().Exec(sql); err != nil {
			t.Fatal(err)
		}
	}
	for _, mutation := range []struct {
		name string
		run  func() error
	}{
		{"metadata", func() error {
			title := "after"
			return s.Update(t.Context(), "records", consoleapi.ConversationPatch{Title: &title})
		}},
		{"one queued exchange", func() error { _, err := s.EditQueued("queued", "after"); return err }},
		{"front insertion", func() error {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.exchanges["console:records"] = append([]*queuedExchange{{Exchange: Exchange{ID: "front", Conversation: "console:records", State: consoleapi.ExchangeQueued}}}, s.exchanges["console:records"]...)
			return s.save()
		}},
		{"append exchange", func() error {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.exchanges["console:records"] = append(s.exchanges["console:records"], &queuedExchange{Exchange: Exchange{ID: "tail", Conversation: "console:records", State: consoleapi.ExchangeQueued}})
			return s.save()
		}},
		{"one reply", func() error {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.replies["console:records"][0].Text = "changed evidence"
			return s.save()
		}},
		{"one question", func() error {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.questions["question"] = consoleapi.PendingQuestion{ID: "question", Conversation: "console:records", State: "pending"}
			return s.save()
		}},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			if _, err := book.DB().Exec(`DELETE FROM console_write_audit`); err != nil {
				t.Fatal(err)
			}
			if err := mutation.run(); err != nil {
				t.Fatal(err)
			}
			var rows, payload int
			if err := book.DB().QueryRow(`SELECT count(*),coalesce(sum(bytes),0) FROM console_write_audit`).Scan(&rows, &payload); err != nil {
				t.Fatal(err)
			}
			if rows == 0 || rows > 4 || payload > 4096 {
				t.Fatalf("small write rewrote history: rows=%d payload=%d, want <=4 rows and <=4096 bytes", rows, payload)
			}
			t.Logf("history=10000 mutation=%s rows=%d payload=%d bytes", mutation.name, rows, payload)
		})
	}
}

func TestConsoleRecordRejectedMetadataIsNotPublishedOrInstalled(t *testing.T) {
	s, book := consoleRecordFixture(t, 1)
	model := readmodel.New(readmodel.Sources{})
	s.model = model
	events, stop := model.Subscribe(t.Context())
	defer stop()
	before := s.meta["console:records"]
	if _, err := book.DB().Exec(`CREATE TRIGGER reject_console_write BEFORE UPDATE ON bindings BEGIN SELECT RAISE(ABORT, 'injected console write refusal'); END`); err != nil {
		t.Fatal(err)
	}
	title := "must not become visible"
	if err := s.Update(t.Context(), "records", consoleapi.ConversationPatch{Title: &title}); err == nil {
		t.Fatal("metadata mutation swallowed the persistence error")
	}
	if !reflect.DeepEqual(s.meta["console:records"], before) {
		t.Fatal("failed metadata mutation remained visible")
	}
	select {
	case e := <-events:
		t.Fatalf("failed metadata mutation published %+v", e)
	default:
	}
}

func storeConsoleState(t testing.TB, book *ledger.Ledger, state DurableState) {
	t.Helper()
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error { return StoreStateTx(tx, state) }); err != nil {
		t.Fatal(err)
	}
}

func TestConsoleRecordsReopenPreservesOrderReceiptsAndRecovery(t *testing.T) {
	dir := t.TempDir()
	book, err := ledger.Open(dir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	const conv = "console:reopen"
	saved := DurableState{
		Replies: map[string][]consoleapi.Reply{
			conv:            {{ID: "r-z", Conversation: conv, Text: "first"}, {ID: "r-a", Conversation: conv, Text: "second"}},
			"console:empty": {},
		},
		Meta: map[string]Meta{conv: {Title: "retained"}},
		Exchanges: map[string][]DurableExchange{conv: {
			{Exchange: Exchange{ID: "e-z", Conversation: conv, Key: "stable", State: consoleapi.ExchangeDone}, PayloadHash: "payload", QuoteAliases: map[string]string{"quote": "source"},
				Receipt: &consoleapi.Reply{ID: "receipt", ExchangeID: "e-z", Conversation: conv, Text: "receipt survives pruning"}, ContinuationRejected: true,
				RecoveryStopTarget: &recoveryStopTarget{Conversation: conv, ExchangeID: "target", TaskID: "task", Requester: "owner"},
				RecoveryStop:       &consoleapi.Reply{ID: "stopped", Conversation: conv, Text: "stop evidence"}, RecoveryStopPending: "pending evidence"},
			{Exchange: Exchange{ID: "e-a", Conversation: conv, Key: "running", State: consoleapi.ExchangeRunning, Input: "work"}},
			{Exchange: Exchange{ID: "deferred", Conversation: conv, State: consoleapi.ExchangeQueued}, RecoveryPending: true},
		}},
		Questions: map[string]consoleapi.PendingQuestion{"q": {ID: "q", Conversation: conv, State: "pending"}},
	}
	storeConsoleState(t, book, saved)
	if err := book.Close(); err != nil {
		t.Fatal(err)
	}
	book, err = ledger.Open(dir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	got, err := LoadState(book)
	if err != nil || !reflect.DeepEqual(got, saved) {
		t.Fatalf("SQLite reopen changed durable owner state: got=%+v err=%v", got, err)
	}
	s := New(nil, "owner", nil)
	if err := s.PersistLedger(book); err != nil {
		t.Fatal(err)
	}
	got, err = LoadState(book)
	if err != nil {
		t.Fatal(err)
	}
	list := got.Exchanges[conv]
	if !reflect.DeepEqual(list[0], saved.Exchanges[conv][0]) || list[1].ID != "e-a" || list[1].State != consoleapi.ExchangeFailed || list[1].Receipt == nil ||
		list[2].ID != "deferred" || !list[2].RecoveryPending || list[2].State != consoleapi.ExchangeQueued || got.Questions["q"].State != "interrupted" {
		t.Fatalf("startup lost receipts/order/recovery obligations: %+v", got)
	}
	if got.Replies[conv][0].ID != "r-z" || got.Replies[conv][1].ID != "r-a" || got.Replies["console:empty"] == nil {
		t.Fatal("startup lost reply order or empty conversation")
	}
	revision := s.records.revision
	if err := s.save(); err != nil || s.records.revision != revision {
		t.Fatal("no-op startup state is not stable", err)
	}
}

func TestConsoleRecordCommitRefusalRollsBackRowsAndShadow(t *testing.T) {
	s, book := consoleRecordFixture(t, 1)
	before, err := LoadState(book)
	if err != nil {
		t.Fatal(err)
	}
	revision := s.records.revision
	if _, err := book.DB().Exec(`CREATE TRIGGER reject_console_control BEFORE UPDATE ON bindings WHEN new.kind='console-store' BEGIN SELECT RAISE(ABORT, 'reject final record'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EditQueued("queued", "must rollback"); err == nil {
		t.Fatal("control refusal did not fail mutation")
	}
	after, err := LoadState(book)
	if err != nil || !reflect.DeepEqual(before, after) || s.records.revision != revision || s.exchanges["console:records"][1].Input != "before" {
		t.Fatal("late transaction failure changed rows, memory or comparison image", err)
	}
	if _, err := book.DB().Exec(`DROP TRIGGER reject_console_control`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EditQueued("queued", "retry"); err != nil {
		t.Fatal(err)
	}
	after, err = LoadState(book)
	if err != nil || after.Exchanges["console:records"][1].Input != "retry" || s.records.revision != revision+1 {
		t.Fatal("retry was incorrectly treated as already committed", err)
	}
}

func TestConsoleRecordStartupFailureInstallsNothingAndCanRetry(t *testing.T) {
	_, book := consoleRecordFixture(t, 0)
	saved, err := LoadState(book)
	if err != nil {
		t.Fatal(err)
	}
	saved.Exchanges["console:records"][0].State = consoleapi.ExchangeRunning
	saved.Questions["q"] = consoleapi.PendingQuestion{ID: "q", State: "pending"}
	storeConsoleState(t, book, saved)
	if _, err := book.DB().Exec(`CREATE TRIGGER reject_console_startup BEFORE UPDATE ON bindings WHEN new.kind='console-store' BEGIN SELECT RAISE(ABORT, 'startup refusal'); END`); err != nil {
		t.Fatal(err)
	}
	s := New(nil, "owner", nil)
	s.replies["console:local"] = []consoleapi.Reply{{ID: "local", Conversation: "console:local", Text: "preexisting"}}
	if err := s.PersistLedger(book); err == nil {
		t.Fatal("startup ignored durable recovery refusal")
	}
	if s.book != nil || len(s.exchanges) != 0 || len(s.replies) != 1 || s.replies["console:local"][0].Text != "preexisting" {
		t.Fatal("failed startup installed partially recovered state")
	}
	got, err := LoadState(book)
	if err != nil || !reflect.DeepEqual(got, saved) {
		t.Fatal("failed recovery changed durable records", err)
	}
	if _, err := book.DB().Exec(`DROP TRIGGER reject_console_startup`); err != nil {
		t.Fatal(err)
	}
	if err := s.PersistLedger(book); err != nil {
		t.Fatal(err)
	}
	if len(s.replies["console:local"]) != 1 {
		t.Fatal("successful startup discarded preexisting local replies")
	}
}

func TestConsoleRecordStaleOwnerFailsWithoutPublishingMutation(t *testing.T) {
	first, book := consoleRecordFixture(t, 0)
	second := New(nil, "owner", nil)
	if err := second.PersistLedger(book); err != nil {
		t.Fatal(err)
	}
	title := "first"
	if err := first.Update(t.Context(), "records", consoleapi.ConversationPatch{Title: &title}); err != nil {
		t.Fatal(err)
	}
	title = "stale"
	if err := second.Update(t.Context(), "records", consoleapi.ConversationPatch{Title: &title}); !errors.Is(err, ledger.ErrConflict) {
		t.Fatalf("stale owner overwrote committed records: %v", err)
	}
	if second.meta["console:records"].Title != "before" {
		t.Fatal("stale mutation remained visible")
	}
}

func TestConsoleRecordComparisonImageDoesNotAliasInstalledPointers(t *testing.T) {
	_, book := consoleRecordFixture(t, 1)
	s := New(nil, "owner", nil)
	if err := s.PersistLedger(book); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"first pointer mutation", "second pointer mutation"} {
		s.mu.Lock()
		s.exchanges["console:records"][0].Receipt.Text = text
		err := s.save()
		s.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		durable, err := LoadState(book)
		if err != nil || durable.Exchanges["console:records"][0].Receipt.Text != text {
			t.Fatal("mutating installed receipt also mutated the committed comparison image", err)
		}
	}
}

func TestConsoleRecordStoreDoesNotReadOrMigrateLegacyDocument(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	raw := []byte(`{"replies":{"console:old":[{"id":"old","conversation":"console:old"}]}}`)
	if err := book.Document("console").Save(raw); err != nil {
		t.Fatal(err)
	}
	s := New(nil, "owner", nil)
	if err := s.Persist(book.Document("console")); err == nil {
		t.Fatal("ledger document was accepted as a production adapter")
	}
	if err := s.PersistLedger(book); err != nil {
		t.Fatal(err)
	}
	if len(s.replies) != 0 {
		t.Fatal("legacy document was loaded or migrated")
	}
	got, _, err := book.Document("console").Load()
	if err != nil || string(got) != string(raw) {
		t.Fatal("records path dual-wrote legacy document", err)
	}
}

func TestConsoleRecordsAndCompletionFailClosedOnCorruption(t *testing.T) {
	for _, tc := range []struct {
		name, sql string
	}{
		{"missing control", `DELETE FROM bindings WHERE kind='console-store'`},
		{"invalid head field", `UPDATE bindings SET data='{"meta":{"title":123}}' WHERE kind='console-head'`},
		{"null head", `UPDATE bindings SET data=' null ' WHERE kind='console-head'`},
		{"missing link", `DELETE FROM bindings WHERE kind='console-exchange' AND id='queued'`},
		{"cycle", `UPDATE bindings SET data=json_set(data,'$.next','queued') WHERE kind='console-exchange' AND id='queued'`},
		{"orphan", `DELETE FROM bindings WHERE kind='console-head'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, book := consoleRecordFixture(t, 0)
			if _, err := book.DB().Exec(tc.sql); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadState(book); err == nil {
				t.Error("corrupt records loaded")
			}
			if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
				return CheckTaskCompletionTx(tx, map[string]bool{"root": true}, "unrelated", "")
			}); err == nil {
				t.Error("corrupt owner state admitted completion")
			}
		})
	}
}

func BenchmarkConsoleSmallWrite10k(b *testing.B) {
	s, book := consoleRecordFixture(b, 10000)
	for _, mode := range []string{"records", "whole-document-baseline"} {
		b.Run(mode, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			payload := 0
			for i := 0; i < b.N; i++ {
				meta := s.meta["console:records"]
				meta.Title = fmt.Sprintf("title %d", i)
				s.meta["console:records"] = meta
				var err error
				if mode == "records" {
					err = s.save()
				} else {
					// Reproduce the previous writer against the same real
					// SQLite backend. This is a benchmark, not a service
					// adapter, dual write or migration path.
					var raw []byte
					raw, err = json.Marshal(transcript{Replies: s.replies, Meta: s.meta, Exchanges: s.exchanges, Questions: s.questions})
					payload = len(raw)
					if err == nil {
						err = book.Document("console-benchmark").Save(raw)
					}
				}
				if err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			if payload != 0 {
				b.ReportMetric(float64(payload), "payload-B/op")
			}
		})
	}
}
