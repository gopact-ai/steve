package intent

import (
	"errors"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

func TestCompletionGuardUsesIntentOperationState(t *testing.T) {
	for _, state := range []State{Claimed, Dispatched, Unknown, Succeeded, Failed, "future"} {
		t.Run(string(state), func(t *testing.T) {
			book, err := ledger.Open(t.TempDir(), ledger.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { book.Close() })
			// Payload state is deliberately stale; the operation is authoritative.
			if _, err := book.Begin(t.Context(), "effect", kind, string(Succeeded), "test", Intent{TaskID: "child", State: Failed}); err != nil {
				t.Fatal(err)
			}
			if _, err := book.Begin(t.Context(), "unrelated", kind, string(Unknown), "test", Intent{TaskID: "other"}); err != nil {
				t.Fatal(err)
			}
			rollback := errors.New("rollback test transition")
			err = book.Update(t.Context(), func(tx *ledger.Tx) error {
				if _, err := tx.Exec(`UPDATE operations SET state = ? WHERE id = 'effect'`, state); err != nil {
					return err
				}
				err := CheckTaskCompletionTx(tx, map[string]bool{"root": true, "child": true})
				if state == Succeeded || state == Failed {
					if err != nil {
						t.Errorf("terminal effect: %v", err)
					}
				} else if !errors.Is(err, task.ErrCompleteAttention) {
					t.Errorf("pending effect admitted: %v", err)
				}
				return rollback
			})
			if !errors.Is(err, rollback) {
				t.Fatal(err)
			}
		})
	}
}

func TestCompletionGuardRejectsCorruptIntentEvenIfUnrelatedOrTerminal(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	if _, err := book.Begin(t.Context(), "effect", kind, string(Succeeded), "test", Intent{TaskID: "other"}); err != nil {
		t.Fatal(err)
	}
	if _, err := book.DB().Exec(`UPDATE operations SET data = '{' WHERE id = 'effect'`); err != nil {
		t.Fatal(err)
	}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
		return CheckTaskCompletionTx(tx, map[string]bool{"root": true})
	}); err == nil {
		t.Fatal("unreadable intent admitted completion")
	}
}
