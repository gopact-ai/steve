package task

import (
	"errors"
	"testing"
	"time"
)

func TestCompletionSuppressionRequiresClosedWorkAndAnEndedParent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func([]Task)
		want   error
	}{
		{"done", func([]Task) {}, nil},
		{"cancelled", func(all []Task) { all[2].State = StateCancelled }, nil},
		{"cancelled-parent", func(all []Task) { all[1].State = StateCancelled }, nil},
		{"review-root", func(all []Task) { all[0].State = StateReview }, nil},
		{"missing-result", func(all []Task) { all[2].Result = nil }, ErrCompleteDelivery},
		{"missing-delivery", func(all []Task) { all[2].Delivery = nil }, ErrCompleteDelivery},
		{"pending", func(all []Task) { all[2].Delivery.State = DeliveryPending }, ErrCompleteDelivery},
		{"queued", func(all []Task) { all[2].Delivery.State = DeliveryQueued }, ErrCompleteDelivery},
		{"uncertain", func(all []Task) { all[2].Delivery.State = DeliveryUncertain }, ErrCompleteDelivery},
		{"unknown-delivery", func(all []Task) { all[2].Delivery.State = "future" }, ErrCompleteDelivery},
		{"running", func(all []Task) { all[2].State = StateRunning }, ErrCompleteChildren},
		{"paused", func(all []Task) { all[2].State = StatePaused }, ErrCompleteChildren},
		{"failed", func(all []Task) { all[2].State = StateFailed }, ErrCompleteChildren},
		{"unknown-task-state", func(all []Task) { all[2].State = "future" }, ErrCompleteChildren},
		{"active-accounting", func(all []Task) { all[2].Attempts = []Attempt{{StartedAt: time.Now()}} }, ErrCompleteBusy},
		{"old-open-accounting", func(all []Task) {
			all[2].ExecutionEpoch = 2
			all[2].Attempts = []Attempt{{StartedAt: time.Now(), ExecutionEpoch: 1}}
		}, ErrCompleteBusy},
		{"live-parent", func(all []Task) { all[2].Parent = all[0].ID }, ErrCompleteDelivery},
		{"not-delegated", func(all []Task) { all[2].Origin = "" }, ErrCompleteDelivery},
		{"handled-failure", func(all []Task) {
			all[2].State, all[2].Settlement, all[2].Result, all[2].Delivery = StateFailed, SettlementHandled, nil, nil
		}, nil},
		{"ignored-failure", func(all []Task) {
			all[2].State, all[2].Settlement, all[2].Result, all[2].Delivery = StateFailed, SettlementIgnored, nil, nil
		}, nil},
		{"settled-but-open-accounting", func(all []Task) {
			all[2].State, all[2].Settlement = StateFailed, SettlementHandled
			all[2].Attempts = []Attempt{{StartedAt: time.Now()}}
		}, ErrCompleteBusy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			all := []Task{
				{ID: "root", State: StateRunning},
				{ID: "child", Parent: "root", Origin: "delegate:root", State: StateDone, Result: &Result{}, Delivery: &Delivery{State: DeliveryDelivered}},
				{ID: "grandchild", Parent: "child", Origin: "delegate:child", State: StateDone, Result: &Result{}, Delivery: &Delivery{State: DeliverySuppressed}},
			}
			tc.change(all)
			if err := CompletionBlocker(all[0], all); !errors.Is(err, tc.want) {
				t.Fatalf("blocker = %v, want %v", err, tc.want)
			}
			if eligible := CompletionEligibility(all)[all[0].ID]; eligible != (tc.want == nil) {
				t.Fatalf("eligibility = %v, want %v", eligible, tc.want == nil)
			}
		})
	}
}
