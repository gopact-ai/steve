package project

import (
	"errors"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

func TestCompletionGuardReadsDisclosureInCallerTransaction(t *testing.T) {
	for _, state := range []string{DisclosureProposed, DisclosureApproved, DisclosureDenied, DisclosureInterrupted} {
		t.Run(state, func(t *testing.T) {
			book, err := ledger.Open(t.TempDir(), ledger.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { book.Close() })
			if _, err := book.Begin(t.Context(), "disclosure", kindDisclosureOp, DisclosureApproved, "test", DisclosureRequest{TaskID: "child"}); err != nil {
				t.Fatal(err)
			}
			if _, err := book.Begin(t.Context(), "unrelated", kindDisclosureOp, DisclosureProposed, "test", DisclosureRequest{TaskID: "other"}); err != nil {
				t.Fatal(err)
			}
			rollback := errors.New("rollback test transition")
			err = book.Update(t.Context(), func(tx *ledger.Tx) error {
				if _, err := tx.Exec(`UPDATE operations SET state = ? WHERE id = 'disclosure'`, state); err != nil {
					return err
				}
				err := CheckTaskCompletionTx(tx, map[string]bool{"root": true, "child": true})
				if state == DisclosureProposed {
					if !errors.Is(err, task.ErrCompleteAttention) {
						t.Errorf("pending disclosure admitted: %v", err)
					}
				} else if err != nil {
					t.Errorf("resolved disclosure: %v", err)
				}
				return rollback
			})
			if !errors.Is(err, rollback) {
				t.Fatal(err)
			}
		})
	}
}

func TestCompletionGuardRejectsCorruptDisclosureEvenIfUnrelatedOrResolved(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	if _, err := book.Begin(t.Context(), "disclosure", kindDisclosureOp, DisclosureApproved, "test", DisclosureRequest{TaskID: "other"}); err != nil {
		t.Fatal(err)
	}
	if _, err := book.DB().Exec(`UPDATE operations SET data = '{' WHERE id = 'disclosure'`); err != nil {
		t.Fatal(err)
	}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
		return CheckTaskCompletionTx(tx, map[string]bool{"root": true})
	}); err == nil {
		t.Fatal("unreadable disclosure admitted completion")
	}
}
