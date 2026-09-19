package ledger

import (
	"errors"
	"testing"
	"time"
)

func TestReadTransactionKeepsOneCommittedSnapshotWithoutWriterAuthority(t *testing.T) {
	book := open(t, t.TempDir(), &clock{t: time.Now()})
	book.db.SetMaxOpenConns(2)
	if err := book.PutBinding(t.Context(), "read-test", "one", 1); err != nil {
		t.Fatal(err)
	}
	read := func(tx *ReadTx) string {
		t.Helper()
		var data string
		if err := tx.QueryRow(`SELECT data FROM bindings WHERE kind=? AND id=?`, "read-test", "one").Scan(&data); err != nil {
			t.Fatal(err)
		}
		return data
	}
	if err := book.Read(t.Context(), func(tx *ReadTx) error {
		if got := read(tx); got != "1" {
			t.Fatal(got)
		}
		if err := book.PutBinding(t.Context(), "read-test", "one", 2); err != nil {
			return err
		}
		if got := read(tx); got != "1" {
			t.Fatal("mixed committed versions in one read transaction", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := book.requireReplication(); err != nil {
		t.Fatal(err)
	}
	if err := book.Update(t.Context(), func(*Tx) error { return nil }); !errors.Is(err, ErrReplicaUnavailable) {
		t.Fatal("fixture is not a read-only follower", err)
	}
	if err := book.Read(t.Context(), func(tx *ReadTx) error {
		if got := read(tx); got != "2" {
			t.Fatal("follower cannot read latest committed snapshot", got)
		}
		return nil
	}); err != nil {
		t.Fatal("read required coordinator/write authority", err)
	}
}

func TestReadTransactionCannotExecuteMutationsThroughQueryMethods(t *testing.T) {
	book := open(t, t.TempDir(), &clock{t: time.Now()})
	if err := book.Read(t.Context(), func(tx *ReadTx) error {
		if err := tx.QueryRow(`DELETE FROM bindings RETURNING id`).Scan(new(string)); !errors.Is(err, ErrReplicaWriteBypass) {
			t.Fatalf("read QueryRow accepted mutation: %v", err)
		}
		if _, err := tx.Query(`INSERT INTO bindings(kind,id,data,updated_at) VALUES('x','x','{}','x') RETURNING id`); !errors.Is(err, ErrReplicaWriteBypass) {
			t.Fatalf("read Query accepted mutation: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
