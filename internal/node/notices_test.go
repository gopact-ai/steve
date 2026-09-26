package node

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// lockedBuffer is a log sink the queue's own goroutine may write to.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) records(t *testing.T) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	dec := json.NewDecoder(bytes.NewReader(b.buf.Bytes()))
	for dec.More() {
		var rec map[string]any
		if err := dec.Decode(&rec); err != nil {
			t.Fatal(err)
		}
		out = append(out, rec)
	}
	return out
}

func captureLogs(t *testing.T) *lockedBuffer {
	t.Helper()
	sink := &lockedBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(sink, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return sink
}

// Observers stuck behind a history write that does not end must not make
// the queue grow without end: it keeps the newest changes, forgets the
// oldest, and says how many it has forgotten. The newest are what history
// ends on and what the machines are now.
func TestStuckObserverLeavesOnlyTheNewestChangesQueued(t *testing.T) {
	const held = 1024
	logs := captureLogs(t)
	var n notices
	t.Cleanup(n.close)
	gate := newObserverGate(t)
	n.post(gate.hold)
	gate.waitEntered(t)
	var mu sync.Mutex
	var heard []int
	for i := 1; i <= held+100; i++ {
		n.post(func() { mu.Lock(); defer mu.Unlock(); heard = append(heard, i) })
	}
	n.mu.Lock()
	queued := len(n.pending)
	n.mu.Unlock()
	if queued != held {
		t.Fatalf("%d changes queued behind a stuck observer, want at most %d", queued, held)
	}
	delivered := make(chan struct{})
	n.post(func() { close(delivered) })
	gate.open()
	select {
	case <-delivered:
	case <-time.After(10 * time.Second):
		t.Fatal("the newest change was never delivered")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(heard) != held-1 || heard[0] != 102 || heard[len(heard)-1] != held+100 {
		t.Fatalf("heard %d changes, want the newest %d: 102 to %d", len(heard), held-1, held+100)
	}
	var dropped float64
	for _, rec := range logs.records(t) {
		if rec["level"] == "WARN" {
			if d, ok := rec["dropped"].(float64); ok {
				dropped = max(dropped, d)
			}
		}
	}
	if dropped != 101 {
		t.Fatalf("logs say %v changes were dropped, want 101", dropped)
	}
}
