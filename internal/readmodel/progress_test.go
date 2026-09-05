package readmodel

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/view"
)

func TestDelegateProgressPreservesReasoningPlanAndModel(t *testing.T) {
	m := New(Sources{})
	events, stop := m.Subscribe(t.Context())
	defer stop()
	reasoning := "开头\n" + strings.Repeat("完整的思考摘要\n", 2000) + "\n[… 省略 123 字节 …]\n结尾"
	p := view.Progress{Reasoning: reasoning, Settings: view.Settings{Model: "reported-model"},
		Plan: []view.Step{{Text: "verify", Status: view.StepCompleted}}}
	m.DelegateProgress("59", "builder", "node-a", StepInfo{State: "done", Answer: "verified"}, p)
	ev := <-events
	if ev.Progress.Reasoning != reasoning || ev.Progress.Model != "reported-model" || ev.Progress.Agent != "builder" || ev.Progress.Node != "node-a" || len(ev.Progress.Plan) != 1 {
		t.Fatalf("delegate progress lost the agent's trace: %+v", ev.Progress)
	}
	if ev.Step == nil || ev.Step.ID != "#59" || ev.Step.Kind != "delegate" || ev.Step.Model != "reported-model" || ev.Step.Plan[0].Text != "verify" || ev.Step.Reasoning != reasoning || ev.Step.Answer != "verified" {
		t.Fatalf("step snapshot differs from progress: %+v", ev.Step)
	}
	data, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Step struct{ ID, Kind, Model, Answer string }
	}
	if err := json.Unmarshal(data, &wire); err != nil || wire.Step.ID != "#59" || wire.Step.Kind != "delegate" || wire.Step.Model != "reported-model" || wire.Step.Answer != "verified" {
		t.Fatalf("step fields must stay flat on the wire: %s (%v)", data, err)
	}
}
