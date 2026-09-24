package ledger

import (
	"database/sql"
	"testing"
	"time"
)

// restoredBinding restores raw into a fresh ledger and reads one binding.
func restoredBinding(t *testing.T, raw []byte, kind, id string) string {
	t.Helper()
	target := open(t, t.TempDir(), &clock{t: time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)})
	if err := target.RestoreReplica(raw); err != nil {
		t.Fatal(err)
	}
	var value string
	if ok, err := target.GetBinding(t.Context(), kind, id, &value); err != nil || !ok {
		t.Fatalf("restored binding %s/%s: ok=%v err=%v", kind, id, ok, err)
	}
	return value
}

// A snapshot copies one committed boundary without holding writes back:
// a write made while its backup is copying finishes, and the snapshot
// holds the state from before it.
func TestWritesFinishWhileAReplicaSnapshotIsCopied(t *testing.T) {
	for _, tc := range []struct {
		name string
		book func(*testing.T) *Ledger
	}{
		{"replicated", func(t *testing.T) *Ledger { l, _ := replicaBook(t); return l }},
		{"standalone", func(t *testing.T) *Ledger {
			return open(t, t.TempDir(), &clock{t: time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := tc.book(t)
			if err := l.PutBinding(t.Context(), "k", "a", "before"); err != nil {
				t.Fatal(err)
			}
			started, release := make(chan struct{}), make(chan struct{})
			l.backupStarted = func() { close(started); <-release }
			type snapshot struct {
				raw []byte
				err error
			}
			taken := make(chan snapshot, 1)
			go func() {
				raw, err := l.SnapshotReplica()
				taken <- snapshot{raw, err}
			}()
			released := false
			t.Cleanup(func() {
				if !released {
					close(release)
					<-taken
				}
			})
			<-started
			within(t, 5*time.Second, "write during a snapshot", func() error {
				return l.PutBinding(t.Context(), "k", "a", "after")
			})
			released = true
			close(release)
			got := <-taken
			if got.err != nil {
				t.Fatal(got.err)
			}
			if value := restoredBinding(t, got.raw, "k", "a"); value != "before" {
				t.Fatalf("snapshot binding = %q, want the state from before the write", value)
			}
		})
	}
}

// A checkpoint fixes its boundary when it is taken. Batches applied after
// it, before or while it is encoded, finish without waiting for it and are
// not in its encoding.
func TestReplicaCheckpointHoldsItsBoundaryWhileBatchesApply(t *testing.T) {
	book, _ := replicaBook(t)
	if err := book.PutBinding(t.Context(), "k", "a", "boundary"); err != nil {
		t.Fatal(err)
	}
	version, err := book.ReplicaVersion()
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := book.SnapshotReplicaCheckpoint(0)
	if err != nil {
		t.Fatal(err)
	}
	defer checkpoint.Release()
	within(t, 5*time.Second, "a batch applied after the boundary", func() error {
		return book.PutBinding(t.Context(), "k", "a", "after the boundary")
	})
	started, release := make(chan struct{}), make(chan struct{})
	book.backupStarted = func() { close(started); <-release }
	type encoded struct {
		raw []byte
		err error
	}
	done := make(chan encoded, 1)
	go func() {
		raw, err := checkpoint.Encode()
		done <- encoded{raw, err}
	}()
	released := false
	t.Cleanup(func() {
		if !released {
			close(release)
			<-done
		}
	})
	<-started
	within(t, 5*time.Second, "a batch applied while the checkpoint is copied", func() error {
		return book.PutBinding(t.Context(), "k", "a", "while encoding")
	})
	released = true
	close(release)
	got := <-done
	if got.err != nil {
		t.Fatal(got.err)
	}
	if value := restoredBinding(t, got.raw, "k", "a"); value != "boundary" {
		t.Fatalf("checkpoint binding = %q, want the state at its boundary", value)
	}
	incarnation, database, err := decodeReplicaSnapshot(got.raw)
	if err != nil {
		t.Fatal(err)
	}
	if incarnation != book.Incarnation() {
		t.Fatalf("checkpoint incarnation = %d, want %d", incarnation, book.Incarnation())
	}
	path, cleanup, err := book.replicaTemp(database)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	copied, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer copied.Close()
	var copiedVersion uint64
	if err := copied.QueryRow(`SELECT version FROM replica_state WHERE singleton=1`).Scan(&copiedVersion); err != nil || copiedVersion != version {
		t.Fatalf("checkpoint replica version = %d (%v), want %d", copiedVersion, err, version)
	}
}

// A checkpoint released without being encoded gives its read connection
// back to the pool.
func TestReleasedReplicaCheckpointsReturnTheirReadConnection(t *testing.T) {
	book, _ := replicaBook(t)
	within(t, 5*time.Second, "checkpoints taken and released in turn", func() error {
		for range 2 * readConnections {
			checkpoint, err := book.SnapshotReplicaCheckpoint(0)
			if err != nil {
				return err
			}
			checkpoint.Release()
			checkpoint.Release()
		}
		return nil
	})
}
