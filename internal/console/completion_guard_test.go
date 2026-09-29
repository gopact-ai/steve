package console

import (
	"errors"
	"testing"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

// completionGuardCase is a console holding at most one question and one
// line, and what a completion check of tasks root and child in conversation
// chat finds in it.
type completionGuardCase struct {
	name     string
	question consoleapi.PendingQuestion
	exchange Exchange
	current  string
	// spare is a close some input asked for, which lets lines queued
	// behind it go on to what follows.
	spare bool
	want  error
	// conversation is a refusal for what the conversation holds, not
	// root or child: ending or cancelling them does not settle it.
	conversation bool
}

func (tc completionGuardCase) saved() DurableState {
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
	return saved
}

var completionGuardCases = []completionGuardCase{
	{name: "empty"},
	{name: "root question", question: consoleapi.PendingQuestion{TaskID: "root", State: "pending"}, want: task.ErrCompleteAttention},
	{name: "child question", question: consoleapi.PendingQuestion{TaskID: "child", Conversation: "elsewhere", State: "pending"}, want: task.ErrCompleteAttention},
	{name: "conversation question", question: consoleapi.PendingQuestion{Conversation: "chat", State: "pending"}, want: task.ErrCompleteAttention, conversation: true},
	{name: "answered", question: consoleapi.PendingQuestion{TaskID: "root", State: "answered"}},
	{name: "unrelated question", question: consoleapi.PendingQuestion{TaskID: "other", Conversation: "elsewhere", State: "pending"}},
	{name: "own command", exchange: Exchange{ID: "current", Conversation: "chat", State: consoleapi.ExchangeRunning}, current: "current"},
	{name: "missing command identity", exchange: Exchange{ID: "current", Conversation: "chat", State: consoleapi.ExchangeRunning}, want: task.ErrCompleteDelivery, conversation: true},
	{name: "another queued command", exchange: Exchange{ID: "other", Conversation: "chat", State: consoleapi.ExchangeQueued}, current: "current", want: task.ErrCompleteDelivery, conversation: true},
	{name: "another running command", exchange: Exchange{ID: "other", Conversation: "chat", State: consoleapi.ExchangeRunning}, current: "current", want: task.ErrCompleteDelivery, conversation: true},
	{name: "recovering", exchange: Exchange{ID: "current", Conversation: "chat", State: consoleapi.ExchangeRecovering}, current: "current", want: task.ErrCompleteAttention, conversation: true},
	{name: "awaiting user", exchange: Exchange{ID: "current", Conversation: "chat", State: consoleapi.ExchangeAwaitingUser}, current: "current", want: task.ErrCompleteAttention, conversation: true},
	{name: "unknown state", exchange: Exchange{ID: "other", Conversation: "chat", State: "future"}, current: "current", want: task.ErrCompleteDelivery, conversation: true},
	{name: "root continuation", exchange: Exchange{ExpectedTask: "root", Conversation: "elsewhere", State: consoleapi.ExchangeQueued}, want: task.ErrCompleteDelivery},
	{name: "child continuation", exchange: Exchange{ExpectedTask: "child", Conversation: "elsewhere", State: consoleapi.ExchangeRunning}, want: task.ErrCompleteDelivery},
	{name: "own continuation is not exempt", exchange: Exchange{ID: "current", ExpectedTask: "root", Conversation: "chat", State: consoleapi.ExchangeRunning}, current: "current", want: task.ErrCompleteDelivery},
	{name: "done", exchange: Exchange{ExpectedTask: "root", Conversation: "chat", State: consoleapi.ExchangeDone}},
	{name: "failed", exchange: Exchange{ExpectedTask: "child", Conversation: "chat", State: consoleapi.ExchangeFailed}},
	{name: "cancelled", exchange: Exchange{ExpectedTask: "root", Conversation: "chat", State: consoleapi.ExchangeCancelled}},
	{name: "unrelated exchange", exchange: Exchange{ExpectedTask: "other", Conversation: "elsewhere", State: consoleapi.ExchangeRunning}},
	{name: "close spares a queued line", exchange: Exchange{ID: "other", Conversation: "chat", State: consoleapi.ExchangeQueued}, current: "current", spare: true},
	{name: "close without a command spares a queued line", exchange: Exchange{ID: "other", Conversation: "chat", State: consoleapi.ExchangeQueued}, spare: true},
	{name: "close spares its own command", exchange: Exchange{ID: "current", Conversation: "chat", State: consoleapi.ExchangeRunning}, current: "current", spare: true},
	{name: "close waits for another running command", exchange: Exchange{ID: "other", Conversation: "chat", State: consoleapi.ExchangeRunning}, current: "current", spare: true, want: task.ErrCompleteDelivery, conversation: true},
	{name: "close waits for an unknown state", exchange: Exchange{ID: "other", Conversation: "chat", State: "future"}, current: "current", spare: true, want: task.ErrCompleteDelivery, conversation: true},
	{name: "close waits for a queued continuation", exchange: Exchange{ID: "other", ExpectedTask: "root", Conversation: "chat", State: consoleapi.ExchangeQueued}, current: "current", spare: true, want: task.ErrCompleteDelivery},
	{name: "close waits for another task's queued continuation", exchange: Exchange{ID: "other", ExpectedTask: "other", Conversation: "chat", State: consoleapi.ExchangeQueued}, current: "current", spare: true, want: task.ErrCompleteDelivery, conversation: true},
	{name: "close waits for recovery", exchange: Exchange{ID: "other", Conversation: "chat", State: consoleapi.ExchangeRecovering}, current: "current", spare: true, want: task.ErrCompleteAttention, conversation: true},
	{name: "close waits for the owner", exchange: Exchange{ID: "other", Conversation: "chat", State: consoleapi.ExchangeAwaitingUser}, current: "current", spare: true, want: task.ErrCompleteAttention, conversation: true},
	{name: "close waits for a question", question: consoleapi.PendingQuestion{Conversation: "chat", State: "pending"}, current: "current", spare: true, want: task.ErrCompleteAttention, conversation: true},
}

func TestCompletionGuardReadsTranscriptInCallerTransaction(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	for _, tc := range completionGuardCases {
		t.Run(tc.name, func(t *testing.T) {
			rollback := errors.New("rollback test transcript")
			err := book.Update(t.Context(), func(tx *ledger.Tx) error {
				if err := StoreStateTx(tx, tc.saved()); err != nil {
					return err
				}
				err := CheckTaskCompletionTx(tx, map[string]bool{"root": true, "child": true}, "chat", tc.current, tc.spare)
				if !errors.Is(err, tc.want) {
					t.Errorf("guard = %v, want %v", err, tc.want)
				}
				if held := errors.Is(err, task.ErrCompleteConversation); held != tc.conversation {
					t.Errorf("guard = %v, the conversation's = %v, want %v", err, held, tc.conversation)
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

// A console read once for several checks judges as each transaction's own
// read would: over the read while the console has not been written since,
// and over what the transaction reads once it has — a question asked or a
// line queued after the read holds the task up, one answered or run lets it
// go.
func TestCompletionReadChecksWhatTheTransactionSees(t *testing.T) {
	for _, tc := range completionGuardCases {
		t.Run(tc.name, func(t *testing.T) {
			for _, readFirst := range []bool{false, true} {
				book, err := ledger.Open(t.TempDir(), ledger.Options{})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { book.Close() })
				before := completionGuardCase{
					question: consoleapi.PendingQuestion{TaskID: "root", State: "pending"},
					exchange: Exchange{ExpectedTask: "child", Conversation: "chat", State: consoleapi.ExchangeRunning},
				}
				if err := book.Update(t.Context(), func(tx *ledger.Tx) error { return StoreStateTx(tx, before.saved()) }); err != nil {
					t.Fatal(err)
				}
				var read *CompletionRead
				if readFirst {
					read = ReadCompletion(book)
				}
				if err := book.Update(t.Context(), func(tx *ledger.Tx) error { return StoreStateTx(tx, tc.saved()) }); err != nil {
					t.Fatal(err)
				}
				if !readFirst {
					read = ReadCompletion(book)
				}
				for range 2 {
					err := book.Update(t.Context(), func(tx *ledger.Tx) error {
						return read.CheckTaskCompletionTx(tx, map[string]bool{"root": true, "child": true}, "chat", tc.current, tc.spare)
					})
					if !errors.Is(err, tc.want) {
						t.Errorf("read before the write %v: guard = %v, want %v", readFirst, err, tc.want)
					}
					if held := errors.Is(err, task.ErrCompleteConversation); held != tc.conversation {
						t.Errorf("read before the write %v: guard = %v, the conversation's = %v, want %v", readFirst, err, held, tc.conversation)
					}
				}
			}
		})
	}
}

// A console that cannot be read admits no completion through a read made
// for several checks either, however the read came to be.
func TestCompletionReadRejectsUnreadableRecords(t *testing.T) {
	const revised = `UPDATE bindings SET data='{"revision":2}' WHERE kind='console-store' AND id='state'`
	for _, tc := range []struct {
		name   string
		writes []string
	}{
		{"store control", []string{`UPDATE bindings SET data='{"revision":0}' WHERE kind='console-store' AND id='state'`}},
		{"broken line chain", []string{`INSERT INTO bindings(kind,id,data,updated_at) VALUES('console-head','chat','{"exchanges":true,"first_exchange":"missing"}','now')`, revised}},
		{"undecodable question", []string{`INSERT INTO bindings(kind,id,data,updated_at) VALUES('console-question','q','{','now')`, revised}},
		{"records without store control", []string{
			`INSERT INTO bindings(kind,id,data,updated_at) VALUES('console-question','q','{"id":"q","task_id":"root","state":"pending"}','now')`,
			`DELETE FROM bindings WHERE kind='console-store'`,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, readFirst := range []bool{false, true} {
				book, err := ledger.Open(t.TempDir(), ledger.Options{})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { book.Close() })
				if err := book.Update(t.Context(), func(tx *ledger.Tx) error { return StoreStateTx(tx, DurableState{}) }); err != nil {
					t.Fatal(err)
				}
				var read *CompletionRead
				if readFirst {
					read = ReadCompletion(book)
				}
				for _, write := range tc.writes {
					if _, err := book.DB().Exec(write); err != nil {
						t.Fatal(err)
					}
				}
				if !readFirst {
					read = ReadCompletion(book)
				}
				for range 2 {
					if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
						return read.CheckTaskCompletionTx(tx, map[string]bool{"root": true}, "chat", "", false)
					}); err == nil {
						t.Fatalf("read before the write %v: unreadable owner records admitted completion", readFirst)
					}
				}
			}
		})
	}
}

// What a task being checked holds itself is what a refusal is about, even
// when the conversation holds something else as well: ending or cancelling
// that task settles it.
func TestCompletionGuardFindsWhatTheTaskHoldsFirst(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	continuation := DurableExchange{Exchange: Exchange{ID: "continuation", ExpectedTask: "root", Conversation: "chat", State: consoleapi.ExchangeQueued}}
	recovering := DurableExchange{Exchange: Exchange{ID: "other", Conversation: "chat", State: consoleapi.ExchangeRecovering}}
	for _, tc := range []struct {
		name  string
		saved DurableState
		want  error
	}{
		{
			name: "its line and the conversation's question",
			saved: DurableState{
				Questions: map[string]consoleapi.PendingQuestion{"q": {ID: "q", Conversation: "chat", State: "pending"}},
				Exchanges: map[string][]DurableExchange{"chat": {continuation}},
			},
			want: task.ErrCompleteDelivery,
		},
		{
			name:  "its line behind one recovering in the conversation",
			saved: DurableState{Exchanges: map[string][]DurableExchange{"chat": {recovering, continuation}}},
			want:  task.ErrCompleteDelivery,
		},
		{
			name: "its question and the conversation's line",
			saved: DurableState{
				Questions: map[string]consoleapi.PendingQuestion{"q": {ID: "q", Conversation: "chat", TaskID: "root", State: "pending"}},
				Exchanges: map[string][]DurableExchange{"chat": {recovering}},
			},
			want: task.ErrCompleteAttention,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rollback := errors.New("rollback test transcript")
			err := book.Update(t.Context(), func(tx *ledger.Tx) error {
				if err := StoreStateTx(tx, tc.saved); err != nil {
					return err
				}
				err := CheckTaskCompletionTx(tx, map[string]bool{"root": true}, "chat", "", true)
				if !errors.Is(err, tc.want) || errors.Is(err, task.ErrCompleteConversation) {
					t.Errorf("guard = %v, want %v held by the task", err, tc.want)
				}
				return rollback
			})
			if !errors.Is(err, rollback) {
				t.Fatal(err)
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
			if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
				return CheckTaskCompletionTx(tx, map[string]bool{"root": true}, "chat", "", false)
			}); err == nil {
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
			return CheckTaskCompletionTx(tx, map[string]bool{"root": true}, "chat", "", false)
		}); err != nil {
			t.Fatal(err)
		}
	}
}
