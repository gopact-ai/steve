package task

import (
	"fmt"
	"testing"
)

func TestCompletionEligibilityKeepsIndependentRootsAndNestedBlockers(t *testing.T) {
	all := []Task{
		{ID: "accepted", State: StateRunning},
		{ID: "blocked", State: StateReview},
		{ID: "child", Parent: "blocked", State: StateDone, Result: &Result{}, Delivery: &Delivery{State: DeliveryDelivered}},
		{ID: "grandchild", Parent: "child", State: StateDone, Result: &Result{}, Delivery: &Delivery{State: DeliveryUncertain}},
		{ID: "closed", State: StateDone},
		{ID: "scheduled", Origin: "schedule:1", State: StateRunning},
	}
	eligible := CompletionEligibility(all)
	if !eligible["accepted"] || eligible["blocked"] || eligible["child"] || eligible["grandchild"] || eligible["closed"] || eligible["scheduled"] {
		t.Fatalf("eligibility lost a tree boundary or blocker: %v", eligible)
	}
	all[3].Delivery.State = DeliveryDelivered
	if eligible = CompletionEligibility(all); !eligible["blocked"] || !eligible["accepted"] {
		t.Fatalf("settled nested receipt did not release its root: %v", eligible)
	}
}

func BenchmarkCompletionEligibilityManyRoots(b *testing.B) {
	for _, count := range []int{1000, 10000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			all := make([]Task, count)
			for i := range all {
				all[i] = Task{ID: fmt.Sprint(i + 1), State: StateRunning}
			}
			b.ResetTimer()
			for b.Loop() {
				if len(CompletionEligibility(all)) != count {
					b.Fatal("lost roots")
				}
			}
		})
	}
}
