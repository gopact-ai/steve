package app

import (
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gopact-ai/steve/internal/task"
)

func TestPlanBudgetReservationsAreAtomicWithoutSyntheticAttempts(t *testing.T) {
	for _, limit := range []int{4, 12} {
		s, err := task.Open(filepath.Join(t.TempDir(), "tasks.json"))
		if err != nil {
			t.Fatal(err)
		}
		tracked, err := s.Create(task.Task{Goal: "parallel plan", Budget: task.Budget{MaxTurns: limit}})
		if err != nil {
			t.Fatal(err)
		}
		var success atomic.Int32
		start := make(chan struct{})
		var workers sync.WaitGroup
		for range 12 {
			workers.Go(func() {
				<-start
				if _, _, err := (taskBudget{s}).Reserve(tracked.ID); err == nil {
					success.Add(1)
				}
			})
		}
		close(start)
		workers.Wait()
		got, _ := s.Get(tracked.ID)
		if success.Load() != int32(limit) || got.Budget.Turns != limit || len(got.Attempts) != 0 {
			t.Fatalf("quota %d: accepted %d, budget %+v, synthetic attempts %d", limit, success.Load(), got.Budget, len(got.Attempts))
		}
	}
}
