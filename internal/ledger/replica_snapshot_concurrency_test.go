package ledger

import (
	"testing"
	"time"
)

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
			replicaBackupStarted = func() { close(started); <-release }
			type snapshot struct {
				raw []byte
				err error
			}
			taken := make(chan snapshot, 1)
			go func() {
				raw, err := l.SnapshotReplica()
				taken <- snapshot{raw, err}
			}()
			t.Cleanup(func() {
				select {
				case <-release:
				default:
					close(release)
				}
				<-taken
				replicaBackupStarted = nil
			})
			<-started
			within(t, 5*time.Second, "write during a snapshot", func() error {
				return l.PutBinding(t.Context(), "k", "a", "after")
			})
			close(release)
			got := <-taken
			taken <- got
			if got.err != nil {
				t.Fatal(got.err)
			}
			target := open(t, t.TempDir(), &clock{t: time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)})
			if err := target.RestoreReplica(got.raw); err != nil {
				t.Fatal(err)
			}
			var value string
			if ok, err := target.GetBinding(t.Context(), "k", "a", &value); err != nil || !ok || value != "before" {
				t.Fatalf("snapshot binding = %q (ok=%v err=%v), want the state from before the write", value, ok, err)
			}
		})
	}
}
