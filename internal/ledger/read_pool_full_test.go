package ledger

import (
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
