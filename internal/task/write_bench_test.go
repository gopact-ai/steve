package task

import (
	"fmt"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
)

// BenchmarkTaskWriteHistory times public single-task writes against a store
// holding a large finished history. Fixture creation is outside the timer.
func BenchmarkTaskWriteHistory(b *testing.B) {
	for _, count := range []int{10000, 100000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			s, target := historyStore(b, count)
			b.Run("meta", func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					title := fmt.Sprint("title ", i)
					if _, err := s.SetMeta(target, MetaPatch{Title: &title}); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("task", func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if err := s.AddInterimForTask(target, fmt.Sprint("message ", i)); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("lineage", func(b *testing.B) {
				child, err := s.Spawn(target, Task{Goal: "child", Channel: "live"})
				if err != nil {
					b.Fatal(err)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := s.Begin(child.ID, "m", "n", ""); err != nil {
						b.Fatal(err)
					}
					if _, err := s.Finish(child.ID, OutcomeOK, Tokens{Total: 1}, 1); err != nil {
						b.Fatal(err)
					}
				}
			})
		})
	}
}

// historyStore holds count finished tasks, each with one settled attempt,
// and one running task whose id it returns.
func historyStore(b *testing.B, count int) (*Store, string) {
	b.Helper()
	book, err := ledger.Open(b.TempDir(), ledger.Options{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = book.Close() })
	s, err := OpenLedger(book)
	if err != nil {
		b.Fatal(err)
	}
	next := s.clone()
	at := time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC)
	for i := range count {
		id := fmt.Sprint(i + 1)
		next.Tasks[id] = &Task{ID: id, State: StateDone, Channel: "past", ProjectID: "p", UpdatedAt: at, ExecutionEpoch: 1,
			Attempts: []Attempt{{ExecutionID: id, StartedAt: at, EndedAt: at.Add(time.Second), Tokens: Tokens{Total: 10}}}}
	}
	next.NextID = count + 1
	if err := s.replaceData(next); err != nil {
		b.Fatal(err)
	}
	target, err := s.Create(Task{Goal: "live", Channel: "live", ProjectID: "p"})
	if err != nil {
		b.Fatal(err)
	}
	if _, err := s.Advance(target.ID, StateRunning); err != nil {
		b.Fatal(err)
	}
	return s, target.ID
}
