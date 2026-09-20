package config

import (
	"sync"
	"testing"
)

func TestRuntimeSettingsReadersObserveWholeSnapshots(t *testing.T) {
	first := &Config{Gateway: Gateway{TaskMaxTurns: 7}, Policies: Policies{Planning: PlanningPolicy{Attempts: 7}}}
	second := &Config{Gateway: Gateway{TaskMaxTurns: 9}, Policies: Policies{Planning: PlanningPolicy{Attempts: 9}}}
	s := NewRuntimeSettings(first)
	var wg sync.WaitGroup
	wg.Go(func() {
		for range 1000 {
			s.Publish(second)
			s.Publish(first)
		}
	})
	for range 4 {
		wg.Go(func() {
			for range 1000 {
				v := s.Load()
				if v.Gateway.TaskMaxTurns != v.Policies.Planning.Attempts {
					t.Error("reader observed a partially published policy")
					return
				}
			}
		})
	}
	wg.Wait()
}
