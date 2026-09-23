package task

import (
	"context"
	"fmt"
	"github.com/gopact-ai/steve/internal/ledger"
	"testing"
	"time"
)

// Baseline reproduces the pre-read-index commit path against the same SQLite
// backend. Fixture creation and startup indexing are outside timed sections.
func BenchmarkTaskSmallWriteHistory(b *testing.B) {
	for _, count := range []int{10000, 100000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			book, err := ledger.Open(b.TempDir(), ledger.Options{})
			if err != nil {
				b.Fatal(err)
			}
			defer book.Close()
			s, err := OpenLedger(book)
			if err != nil {
				b.Fatal(err)
			}
			next := s.clone()
			at := time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC)
			next.Tasks["target"] = &Task{ID: "target", State: StateRunning, Channel: "live", ProjectID: "p", UpdatedAt: at}
			for i := range count {
				id := fmt.Sprintf("past-%06d", i)
				next.Tasks[id] = &Task{ID: id, State: StateDone, Channel: "past", ProjectID: "p", UpdatedAt: at,
					Attempts: []Attempt{{ExecutionID: id, StartedAt: at, EndedAt: at.Add(time.Second), Tokens: Tokens{Total: 10}}}}
			}
			if err := s.replaceLocked(next); err != nil {
				b.Fatal(err)
			}
			for _, mode := range []string{"records-baseline", "indexed"} {
				b.Run(mode, func(b *testing.B) {
					s.rebuildReadIndexLocked()
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						next := s.clone()
						next.Tasks["target"].UpdatedAt = s.data.Tasks["target"].UpdatedAt.Add(time.Second)
						next.Tasks["target"].Result = &Result{Answer: fmt.Sprint(i)}
						var err error
						if mode == "indexed" {
							err = s.replaceLocked(next)
						} else {
							err = benchmarkRecordWriteWithoutReadIndex(s, next)
						}
						if err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		})
	}
}
func benchmarkRecordWriteWithoutReadIndex(s *Store, next data) error {
	changes, err := recordChanges(s.data, next)
	if err != nil {
		return err
	}
	changed := len(changes) > 0 || next.NextID != s.data.NextID
	err = s.book.Update(context.Background(), func(tx *ledger.Tx) error {
		control, err := controlTx(tx)
		if err != nil {
			return err
		}
		if control.Revision != s.revision || control.NextID != s.data.NextID {
			return ledger.ErrConflict
		}
		if !changed {
			return nil
		}
		return writeRecordChangesTx(tx, changes, next.NextID, s.revision)
	})
	if err != nil {
		return err
	}
	if changed {
		s.revision++
	}
	s.data = next
	return nil
}
