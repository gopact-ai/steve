package ledger

import (
	"context"
	"testing"
	"time"
)

// fillReadPool holds every pooled read connection open until the test ends.
func fillReadPool(t *testing.T, l *Ledger) {
	t.Helper()
	for range readConnections {
		holdRead(t, l, func(tx *ReadTx) error {
			_, err := bindingValue(tx, "k", "a")
			return err
		})
	}
}

// Commit and apply paths read on the writer connection. With every pooled
// read connection held open, effect writes, replicated writes, their apply
// and a replica restore still finish.
func TestCommitPathsFinishWithTheReadPoolFull(t *testing.T) {
	t.Run("standalone effect", func(t *testing.T) {
		l := open(t, t.TempDir(), &clock{t: time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)})
		fillReadPool(t, l)
		within(t, 5*time.Second, "effect write", func() error {
			_, err := l.Journal().Started(EffectID{Operation: "op-1", Kind: "message", InstanceKey: "1"}, "cmd", map[string]string{"a": "b"})
			return err
		})
	})
	t.Run("replicated write and effect", func(t *testing.T) {
		l, _ := replicaBook(t)
		fillReadPool(t, l)
		within(t, 5*time.Second, "replicated write", func() error {
			return l.PutBinding(t.Context(), "k", "b", "value")
		})
		within(t, 5*time.Second, "replicated effect write", func() error {
			_, err := l.Journal().Started(EffectID{Operation: "op-1", Kind: "message", InstanceKey: "1"}, "cmd", nil)
			return err
		})
	})
	t.Run("restore", func(t *testing.T) {
		source, _ := replicaBook(t)
		if _, err := source.Journal().Started(EffectID{Operation: "op-1", Kind: "message", InstanceKey: "1"}, "cmd", nil); err != nil {
			t.Fatal(err)
		}
		snapshot, err := source.SnapshotReplica()
		if err != nil {
			t.Fatal(err)
		}
		target := open(t, t.TempDir(), &clock{t: time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)})
		fillReadPool(t, target)
		within(t, 5*time.Second, "restore", func() error { return target.RestoreReplica(snapshot) })
	})
}

// Export and import read the effect evidence from the read pool. They must
// not hold the journal lock while waiting for a pooled connection: an effect
// write takes that lock when it syncs the journal after committing, under the
// writer lock.
func TestTransferWaitingForAReadDoesNotHoldEffectWrites(t *testing.T) {
	for _, tc := range []struct {
		name     string
		transfer func(*Ledger) error
	}{
		{"export", func(l *Ledger) error {
			_, err := l.ExportOperations(context.Background(), nil)
			return err
		}},
		{"import", func(l *Ledger) error {
			return l.ImportFacts(context.Background(), TransferFacts{}, nil, nil)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := open(t, t.TempDir(), &clock{t: time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)})
			transferred := make(chan error, 1)
			// Cleanups run in reverse: the pool is released first, then
			// the transfer finishes, then the ledger closes.
			t.Cleanup(func() {
				if err := <-transferred; err != nil {
					t.Errorf("%s: %v", tc.name, err)
				}
			})
			fillReadPool(t, l)
			waits := l.reads.Stats().WaitCount
			go func() { transferred <- tc.transfer(l) }()
			deadline := time.Now().Add(5 * time.Second)
			for l.reads.Stats().WaitCount == waits {
				if time.Now().After(deadline) {
					t.Fatalf("%s never waited for a pooled connection", tc.name)
				}
				time.Sleep(time.Millisecond)
			}
			within(t, 5*time.Second, "effect write", func() error {
				_, err := l.Journal().Started(EffectID{Operation: "op-1", Kind: "message", InstanceKey: "1"}, "cmd", nil)
				return err
			})
		})
	}
}
