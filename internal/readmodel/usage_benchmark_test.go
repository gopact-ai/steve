package readmodel

import (
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
)

// Keep a busy day's independent task trees in every calendar window. This
// exercises snapshot aggregation rather than ledger or transport overhead.
func BenchmarkUsageSummary(b *testing.B) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	for _, count := range []int{1000, 10000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			tasks := make([]Task, count)
			records := make([]attempt.Record, count)
			for i := range records {
				id := fmt.Sprint(i)
				start := now.Add(-time.Hour).Add(time.Duration(i%3600) * time.Second)
				tasks[i] = Task{ID: id, Title: "Synthetic root task", Origin: "chat", ProjectID: "project"}
				records[i] = attempt.Record{Spec: attempt.Spec{TaskID: id, Agent: fmt.Sprint(i % 10), Project: "project", Harness: "harness"},
					StartedAt: start, EndedAt: start.Add(time.Second), Usage: &attempt.Usage{Reported: true, Input: 100, Output: 20, Model: "model"}}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				runtime.KeepAlive(usage(records, now, tasks))
			}
		})
	}
}
