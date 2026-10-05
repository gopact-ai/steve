package plan

import (
	"testing"

	"github.com/gopact-ai/steve/internal/task"
)

func TestValidatePlanExecutionOwnership(t *testing.T) {
	for _, owner := range []string{"task", "other", ""} {
		p := Plan{TaskID: "task", Execution: &task.ExecutionToken{TaskID: owner, Epoch: 1}, Steps: []Step{{ID: "work", Goal: "work", Agent: "worker", Verify: &Verify{Kind: VerifyNone, Why: "test"}}}}
		if err := Validate(p); (err == nil) != (owner == "task") {
			t.Fatalf("owner %q validation=%v", owner, err)
		}
	}
	p := Plan{Execution: &task.ExecutionToken{TaskID: "task", Epoch: 1}}
	if err := Validate(p); err == nil {
		t.Fatal("taskless plan acquired a task execution")
	}
}
