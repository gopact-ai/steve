package ledger

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// BenchmarkWriteDuringLongRead measures a small write while other callers
// keep a long read scan running over the same ledger.
func BenchmarkWriteDuringLongRead(b *testing.B) {
	l, err := Open(b.TempDir(), Options{})
	if err != nil {
		b.Fatal(err)
	}
	defer l.Close()
	ctx := context.Background()
	if err := l.Update(ctx, func(tx *Tx) error {
		for i := range 1500 {
			if err := tx.PutBinding("scan", fmt.Sprintf("row-%04d", i), i); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		b.Fatal(err)
	}
	stop := make(chan struct{})
	var readers sync.WaitGroup
	for range 2 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = l.Read(ctx, func(tx *ReadTx) error {
					var n int
					return tx.QueryRow(`SELECT COUNT(*) FROM bindings AS a, bindings AS b WHERE a.kind = 'scan' AND b.kind = 'scan'`).Scan(&n)
				})
			}
		}()
	}
	i := 0
	for b.Loop() {
		if err := l.PutBinding(ctx, "write", fmt.Sprintf("w-%d", i), i); err != nil {
			b.Fatal(err)
		}
		i++
	}
	b.StopTimer()
	close(stop)
	readers.Wait()
}
