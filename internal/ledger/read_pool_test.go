package ledger

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

// holdRead opens a read transaction, reads through it once so its snapshot
// is fixed, and keeps it open until the returned release is called.
func holdRead(t *testing.T, l *Ledger, first func(*ReadTx) error) (release func(then func(*ReadTx) error) error) {
	t.Helper()
	started := make(chan error, 1)
	proceed := make(chan func(*ReadTx) error)
	finished := make(chan error, 1)
	go func() {
		finished <- l.Read(context.Background(), func(tx *ReadTx) error {
			if err := first(tx); err != nil {
				started <- err
				return err
			}
			started <- nil
			if then := <-proceed; then != nil {
				return then(tx)
			}
			return nil
		})
	}()
	if err := <-started; err != nil {
		t.Fatal(err)
	}
	released := false
	t.Cleanup(func() {
		if !released {
			proceed <- nil
			<-finished
		}
	})
	return func(then func(*ReadTx) error) error {
		released = true
		proceed <- then
		return <-finished
	}
}

func within(t *testing.T, d time.Duration, what string, fn func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	case <-time.After(d):
		t.Fatalf("%s waited for an open read", what)
	}
}

func bindingValue(q interface {
	QueryRow(string, ...any) *Row
}, kind, id string) (string, error) {
	var data string
	err := q.QueryRow(`SELECT data FROM bindings WHERE kind = ? AND id = ?`, kind, id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return data, err
}

// A long read holds one snapshot; writes commit beside it, and the read
// keeps seeing exactly the state it started with.
func TestWriteCommitsWhileAReadIsOpen(t *testing.T) {
	l := open(t, t.TempDir(), &clock{t: time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)})
	ctx := t.Context()
	if err := l.PutBinding(ctx, "k", "a", "before"); err != nil {
		t.Fatal(err)
	}
	release := holdRead(t, l, func(tx *ReadTx) error {
		got, err := bindingValue(tx, "k", "a")
		if err == nil && got != `"before"` {
			err = errors.New("unexpected first read " + got)
		}
		return err
	})
	within(t, 5*time.Second, "write", func() error {
		if err := l.PutBinding(ctx, "k", "a", "after"); err != nil {
			return err
		}
		return l.Update(ctx, func(tx *Tx) error { return tx.PutBinding("k", "b", "new") })
	})
	// A read begun after the commit sees it, while the open one does not.
	var fresh string
	within(t, 5*time.Second, "second read", func() error {
		var err error
		fresh, err = bindingValue(l.DB(), "k", "a")
		return err
	})
	if fresh != `"after"` {
		t.Fatalf("read after commit = %s", fresh)
	}
	if err := release(func(tx *ReadTx) error {
		a, err := bindingValue(tx, "k", "a")
		if err != nil {
			return err
		}
		b, err := bindingValue(tx, "k", "b")
		if err != nil {
			return err
		}
		if a != `"before"` || b != "" {
			return errors.New("open read saw a later commit: " + a + " " + b)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Restoring a replica replaces the database under readers without waiting
// for them. A reader keeps its snapshot of the old state; the restore
// generation brackets the replacement, and every later read sees the
// restored facts.
func TestRestoreReplicaDoesNotWaitForOpenReads(t *testing.T) {
	source, _ := replicaBook(t)
	ctx := t.Context()
	if err := source.PutBinding(ctx, "k", "a", "restored"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := source.SnapshotReplica()
	if err != nil {
		t.Fatal(err)
	}
	target := open(t, t.TempDir(), &clock{t: time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)})
	if err := target.PutBinding(ctx, "k", "a", "local"); err != nil {
		t.Fatal(err)
	}
	before := target.RestoreGeneration()
	release := holdRead(t, target, func(tx *ReadTx) error {
		_, err := bindingValue(tx, "k", "a")
		return err
	})
	within(t, 5*time.Second, "restore", func() error { return target.RestoreReplica(snapshot) })
	if after := target.RestoreGeneration(); after != before+2 {
		t.Fatalf("restore generation %d -> %d", before, after)
	}
	if err := release(func(tx *ReadTx) error {
		got, err := bindingValue(tx, "k", "a")
		if err == nil && got != `"local"` {
			err = errors.New("open read left its snapshot: " + got)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := target.Read(ctx, func(tx *ReadTx) error {
		got, err := bindingValue(tx, "k", "a")
		if err == nil && got != `"restored"` {
			err = errors.New("read after restore = " + got)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if version, err := target.ReplicaVersion(); err != nil || version != 1 {
		t.Fatalf("replica version after restore = %d, %v", version, err)
	}
	// Derived read indexes serve reads on the restored database.
	if _, _, err := target.HistoryEvents(ctx, nil, nil, 10); err != nil {
		t.Fatal(err)
	}
}
