package readmodel

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

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

func TestTimelineSurvivesProjectionWithEveryToolReference(t *testing.T) {
	at := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	p := view.Progress{Timeline: []view.Span{{Kind: "text", Text: "first", At: at}}}
	for i := range toolsKept + 5 {
		id := fmt.Sprintf("tool-%d", i)
		p.Tools = append(p.Tools, view.Tool{ID: id, Status: view.ToolCompleted})
		p.Timeline = append(p.Timeline, view.Span{Kind: "tool", Tool: id, At: at})
	}
	p.Timeline = append(p.Timeline, view.Span{Kind: "text", Text: "final", At: at})
	progress := FromProgress(p)
	step := FromStepProgress("#72", progress, StepInfo{Kind: "delegate", State: "done", Answer: "legacy answer"})
	raw, err := json.Marshal(step)
	if err != nil {
		t.Fatal(err)
	}
	var restored StepProcess
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored.Timeline, progress.Timeline) || len(restored.Tools) != len(p.Tools) || restored.Answer != "legacy answer" {
		t.Fatal("step snapshot lost timeline order, details, or legacy answer")
	}
	p.Timeline[0].Text = "mutated"
	if progress.Timeline[0].Text != "first" {
		t.Fatal("projection aliased the input timeline")
	}
	if restored.Timeline[len(restored.Timeline)-1].Text != "final" || !strings.Contains(string(raw), `"timeline":[{"kind":"text","text":"first","at":"2026-09-05T12:00:00Z"}`) {
		t.Fatalf("timeline wire shape = %s", raw)
	}
	p.Timeline = nil
	if legacy := FromProgress(p); legacy.Timeline != nil || len(legacy.Tools) != toolsKept {
		t.Fatal("legacy projection changed")
	}
}
