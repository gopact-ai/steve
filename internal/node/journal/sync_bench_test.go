package journal

import (
	"testing"
	"time"
)

// BenchmarkAppendWhileSyncing measures appends while the background sync
// runs at a short interval.
func BenchmarkAppendWhileSyncing(b *testing.B) {
	j, err := New(b.TempDir(), "stream-1", Options{SyncInterval: time.Millisecond})
	if err != nil {
		b.Fatal(err)
	}
	defer j.Close()
	line := []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"text":"hello"}}` + "\n")
	for b.Loop() {
		if _, err := j.Out.Append(line); err != nil {
			b.Fatal(err)
		}
	}
}
