package ledger

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestCommandReceiptTxMatchesOwnerDecoder(t *testing.T) {
	for _, mutation := range []string{
		"",
		`UPDATE commands SET received_at='invalid-date' WHERE id='known'`,
		`UPDATE commands SET finished_at='invalid-date' WHERE id='known'`,
		`UPDATE commands SET error=NULL WHERE id='known'`,
		`UPDATE commands SET error='delivery failed' WHERE id='known'`,
		`UPDATE commands SET result=NULL WHERE id='known'`,
	} {
		t.Run(mutation, func(t *testing.T) {
			book := historyBook(t)
			if err := book.RecordCommand(t.Context(), "known", "reply", "owner", json.RawMessage(`{"receipt":"delivered"}`)); err != nil {
				t.Fatal(err)
			}
			if _, err := book.DB().Exec(`INSERT INTO commands(id,kind,actor,received_at) VALUES ('pending','reply','owner','2026-09-19T00:00:00Z')`); err != nil {
				t.Fatal(err)
			}
			if mutation != "" {
				if _, err := book.DB().Exec(mutation); err != nil {
					t.Fatal(err)
				}
			}
			for _, id := range []string{"known", "pending", "missing", ""} {
				want, found, wantErr := book.CommandReceipt(t.Context(), id)
				var got CommandRecord
				var exists bool
				err := book.Read(t.Context(), func(tx *ReadTx) error {
					var err error
					got, exists, err = CommandReceiptTx(tx, id)
					return err
				})
				if (err != nil) != (wantErr != nil) || err != nil && err.Error() != wantErr.Error() ||
					exists != found || !reflect.DeepEqual(got, want) {
					t.Fatalf("%s: Tx reader differs from owner decoder: %+v/%t/%v != %+v/%t/%v", id, got, exists, err, want, found, wantErr)
				}
			}
		})
	}
}

func TestCommandReceiptTxCannotClearUnknownOrChangeIdempotency(t *testing.T) {
	book := historyBook(t)
	raw := json.RawMessage(`{"receipt":"delivered"}`)
	if err := book.RecordCommand(t.Context(), "known", "reply", "owner", raw); err != nil {
		t.Fatal(err)
	}
	if _, err := book.DB().Exec(`INSERT INTO commands(id,kind,actor,received_at) VALUES ('pending','reply','owner','2026-09-19T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := book.Read(t.Context(), func(tx *ReadTx) error {
		for _, id := range []string{"known", "pending", "missing"} {
			if _, _, err := CommandReceiptTx(tx, id); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := book.RecordCommand(t.Context(), "known", "reply", "owner", raw); err != nil {
		t.Fatalf("exact retry changed after a read: %v", err)
	}
	if err := book.RecordCommand(t.Context(), "known", "reply", "other-owner", raw); !errors.Is(err, ErrConflict) {
		t.Fatalf("different actor accepted after a read: %v", err)
	}
	if err := book.RecordCommand(t.Context(), "known", "reply", "owner", json.RawMessage(`{}`)); !errors.Is(err, ErrConflict) {
		t.Fatalf("different result accepted after a read: %v", err)
	}
	if err := book.RecordCommand(t.Context(), "pending", "reply", "owner", raw); !errors.Is(err, ErrInFlight) {
		t.Fatalf("reading an unresolved dispatch allowed retry: %v", err)
	}
	if _, found, err := book.CommandReceipt(t.Context(), "missing"); err != nil || found {
		t.Fatalf("reading missing evidence reserved a command: %t %v", found, err)
	}
}
