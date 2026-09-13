package readmodel

import (
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/task"
)

func TestCompletionEntryRequiresKnownIdleAcceptedRoot(t *testing.T) {
	for _, scenario := range []string{"ready", "child-running", "child-failed", "result-missing", "receipt-pending", "receipt-suppressed", "open-row", "activity-unknown", "attention-unknown", "live", "unsettled", "question", "plan", "descendant-plan", "done"} {
		t.Run(scenario, func(t *testing.T) {
			list := []task.Task{
				{ID: "root", State: task.StateRunning},
				{ID: "child", Parent: "root", Origin: "delegate:root", State: task.StateDone, Result: &task.Result{}, Delivery: &task.Delivery{State: task.DeliveryDelivered}},
			}
			builder := snapshotBuilder{activityKnown: true, attentionKnown: true}
			plans := map[string]plan.Plan{}
			switch scenario {
			case "child-running":
				list[1].State = task.StateRunning
			case "child-failed":
				list[1].State = task.StateFailed
			case "result-missing":
				list[1].Result = nil
			case "receipt-pending":
				list[1].Delivery = nil
			case "receipt-suppressed":
				list[1].Delivery.State = task.DeliverySuppressed
			case "open-row":
				list[0].Attempts = []task.Attempt{{StartedAt: time.Now()}}
			case "activity-unknown":
				builder.activityKnown = false
			case "attention-unknown":
				builder.attentionKnown = false
			case "live", "unsettled":
				builder.snap.Attempts = []Attempt{{TaskID: "root", Unsettled: scenario == "unsettled"}}
			case "question":
				builder.snap.Inbox = []HumanRequest{{TaskID: "root"}}
			case "plan":
				plans["root"] = plan.Plan{ID: "plan"}
			case "descendant-plan":
				plans["child"] = plan.Plan{ID: "plan"}
			case "done":
				list[0].State = task.StateDone
			}
			builder.snap.Tasks = tasks(list, plans)
			builder.taskAxes()
			if builder.snap.Tasks[0].CanComplete != (scenario == "ready") {
				t.Fatalf("%s: %+v", scenario, builder.snap.Tasks[0])
			}
			if builder.snap.Tasks[1].CanComplete {
				t.Fatal("child exposed complete action")
			}
			if scenario == "done" && builder.snap.Tasks[0].Lane != "ended" {
				t.Fatal("completed task missing from ended lane")
			}
		})
	}
}
