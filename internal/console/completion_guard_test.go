package console

import (
	"errors"
	"testing"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

func TestCompletionGuardReadsTranscriptInCallerTransaction(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	for _, tc := range []struct {
		name     string
		question consoleapi.PendingQuestion
		exchange Exchange
		current  string
		want     error
	}{
		{name: "empty"},
		{name: "root question", question: consoleapi.PendingQuestion{TaskID: "root", State: "pending"}, want: task.ErrCompleteAttention},
		{name: "child question", question: consoleapi.PendingQuestion{TaskID: "child", Conversation: "elsewhere", State: "pending"}, want: task.ErrCompleteAttention},
		{name: "conversation question", question: consoleapi.PendingQuestion{Conversation: "chat", State: "pending"}, want: task.ErrCompleteAttention},
		{name: "answered", question: consoleapi.PendingQuestion{TaskID: "root", State: "answered"}},
		{name: "unrelated question", question: consoleapi.PendingQuestion{TaskID: "other", Conversation: "elsewhere", State: "pending"}},
		{name: "own command", exchange: Exchange{ID: "current", Conversation: "chat", State: consoleapi.ExchangeRunning}, current: "current"},
		{name: "missing command identity", exchange: Exchange{ID: "current", Conversation: "chat", State: consoleapi.ExchangeRunning}, want: task.ErrCompleteDelivery},
		{name: "another queued command", exchange: Exchange{ID: "other", Conversation: "chat", State: consoleapi.ExchangeQueued}, current: "current", want: task.ErrCompleteDelivery},
		{name: "another running command", exchange: Exchange{ID: "other", Conversation: "chat", State: consoleapi.ExchangeRunning}, current: "current", want: task.ErrCompleteDelivery},
		{name: "recovering", exchange: Exchange{ID: "current", Conversation: "chat", State: consoleapi.ExchangeRecovering}, current: "current", want: task.ErrCompleteAttention},
		{name: "awaiting user", exchange: Exchange{ID: "current", Conversation: "chat", State: consoleapi.ExchangeAwaitingUser}, current: "current", want: task.ErrCompleteAttention},
		{name: "unknown state", exchange: Exchange{ID: "other", Conversation: "chat", State: "future"}, current: "current", want: task.ErrCompleteDelivery},
		{name: "root continuation", exchange: Exchange{ExpectedTask: "root", Conversation: "elsewhere", State: consoleapi.ExchangeQueued}, want: task.ErrCompleteDelivery},
		{name: "child continuation", exchange: Exchange{ExpectedTask: "child", Conversation: "elsewhere", State: consoleapi.ExchangeRunning}, want: task.ErrCompleteDelivery},
		{name: "own continuation is not exempt", exchange: Exchange{ID: "current", ExpectedTask: "root", Conversation: "chat", State: consoleapi.ExchangeRunning}, current: "current", want: task.ErrCompleteDelivery},
		{name: "done", exchange: Exchange{ExpectedTask: "root", Conversation: "chat", State: consoleapi.ExchangeDone}},
		{name: "failed", exchange: Exchange{ExpectedTask: "child", Conversation: "chat", State: consoleapi.ExchangeFailed}},
		{name: "cancelled", exchange: Exchange{ExpectedTask: "root", Conversation: "chat", State: consoleapi.ExchangeCancelled}},
		{name: "unrelated exchange", exchange: Exchange{ExpectedTask: "other", Conversation: "elsewhere", State: consoleapi.ExchangeRunning}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			saved := DurableState{}
			if tc.question.State != "" {
				tc.question.ID = "q"
				saved.Questions = map[string]consoleapi.PendingQuestion{"q": tc.question}
			}
			if tc.exchange.State != "" {
				if tc.exchange.ID == "" {
					tc.exchange.ID = "exchange"
				}
				saved.Exchanges = map[string][]DurableExchange{tc.exchange.Conversation: {{Exchange: tc.exchange}}}
			}
			rollback := errors.New("rollback test transcript")
			err := book.Update(t.Context(), func(tx *ledger.Tx) error {
				if err := StoreStateTx(tx, saved); err != nil {
					return err
				}
				err := CheckTaskCompletionTx(tx, map[string]bool{"root": true, "child": true}, "chat", tc.current)
				if !errors.Is(err, tc.want) {
					t.Errorf("guard = %v, want %v", err, tc.want)
				}
				return rollback
			})
			if !errors.Is(err, rollback) {
				t.Fatal(err)
			}
			if records, err := loadConsoleRecords(book); err != nil || records.revision != 0 {
				t.Fatalf("guard committed caller's transaction: revision=%v err=%v", records.revision, err)
			}
		})
	}
}

func TestCompletionGuardRejectsUnreadableRecords(t *testing.T) {
	for _, raw := range []string{`{`, `[]`, `null`, `{"revision":0}`} {
		t.Run(raw, func(t *testing.T) {
			book, err := ledger.Open(t.TempDir(), ledger.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer book.Close()
			if _, err := book.DB().Exec(`INSERT INTO bindings(kind,id,data,updated_at) VALUES('console-store','state',?,'now')`, raw); err != nil {
				t.Fatal(err)
			}
			if err := book.Update(t.Context(), func(tx *ledger.Tx) error { return CheckTaskCompletionTx(tx, map[string]bool{"root": true}, "chat", "") }); err == nil {
				t.Fatal("unreadable owner records admitted completion")
			}
		})
	}
}

func TestCompletionGuardAllowsMissingOrEmptyRecords(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	for _, initialize := range []bool{false, true} {
		if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
			if initialize {
				if err := StoreStateTx(tx, DurableState{}); err != nil {
					return err
				}
			}
			return CheckTaskCompletionTx(tx, map[string]bool{"root": true}, "chat", "")
		}); err != nil {
			t.Fatal(err)
		}
	}
}
