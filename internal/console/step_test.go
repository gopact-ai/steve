package console

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/readmodel"
	"github.com/gopact-ai/steve/internal/turn"
)

func TestChildSnapshotOutlivesItsParentTurn(t *testing.T) {
	model := readmodel.New(readmodel.Sources{})
	doc := &memDoc{}
	h := &queueHandler{started: make(chan *queueCall, 4)}
	s := New(h, "owner", model)
	if err := s.Persist(doc); err != nil {
		t.Fatal(err)
	}
	first, _, err := s.enqueue(t.Context(), "main", "delegate", "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	call := nextCall(t, h)
	step := readmodel.FromStepProgress("#59", readmodel.Progress{
		Agent: "builder", Node: "node-a", Model: "child-model",
		Reasoning: strings.Repeat("完整思考\n", 2000),
		Plan:      []readmodel.PlanLine{{Text: "build", Status: "in_progress"}},
		Tools:     []readmodel.ToolCall{{ID: "tool-1", Name: "build", Status: "running"}},
	}, readmodel.StepInfo{Kind: "delegate", Goal: "build the release", State: "running"})
	s.UpdateStep("main", "59", step)
	other := step
	other.ID = "#60"
	s.UpdateStep("main", "60", other)
	call.finish <- nil
	<-first.done
	if got := first.outcome.reply.Process; got == nil || len(got.Steps) != 2 || got.Steps[0].Model != "child-model" {
		t.Fatalf("parent lost its child snapshots: %+v", got)
	}

	// Late progress while idle updates the existing reply immediately.
	step.Reasoning += "父回合已经结束"
	s.UpdateStep("console:main", "59", step)
	second, _, err := s.enqueue(t.Context(), "main", "a different turn", "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	call = nextCall(t, h)
	oldReplies := s.Replies("main")
	encoded := make(chan struct{})
	go func() {
		defer close(encoded)
		for range 100 {
			_, _ = json.Marshal(oldReplies)
		}
	}()
	step.State, step.Answer, step.Elapsed = "done", "built the release", "2m0s"
	step.Refs, step.Attempt, step.Files = []string{"artifact:release"}, "attempt-59", 2
	step.Plan = []readmodel.PlanLine{{Text: "build", Status: "completed"}}
	step.Tools = []readmodel.ToolCall{{ID: "tool-1", Name: "build", Status: "completed"}}
	for range 100 {
		s.UpdateStep("main", "59", step)
	}
	<-encoded
	if got := oldReplies[1].Process.Steps[0].State; got != "running" {
		t.Fatalf("mutated a reader's snapshot: %s", got)
	}
	call.finish <- nil
	<-second.done
	if got := second.outcome.reply.Process; got != nil && len(got.Steps) != 0 {
		t.Fatalf("old child was attached to the new turn: %+v", got)
	}
	found := false
	for _, ev := range model.Recent() {
		if ev.Kind == "console.step" && ev.TaskID == "59" {
			found = true
			if ev.Conversation != "console:main" || ev.ReplyID != first.ReplyID || ev.Step == nil || ev.Step.Model != "child-model" {
				t.Fatalf("child event has the wrong owner or snapshot: %+v", ev)
			}
		}
	}
	if !found {
		t.Fatal("no console.step event")
	}
	restored := New(nil, "owner", nil)
	if err := restored.Persist(doc); err != nil {
		t.Fatal(err)
	}
	got := restored.Replies("main")[1].Process.Steps
	if len(got) != 2 || got[0].State != "done" || got[0].Reasoning != step.Reasoning || got[0].Model != step.Model || got[0].Plan[0].Status != "completed" || got[0].Files != 2 || got[0].Refs[0] != "artifact:release" || got[1].State != "running" {
		t.Fatalf("restart lost the final child snapshot: %+v", got)
	}
}

type stepHandler func(turn.Request) turn.Result

func (h stepHandler) Handle(_ context.Context, req turn.Request) (turn.Result, error) {
	return h(req), nil
}

type finishingInspector struct{ entered, release chan struct{} }

func (i finishingInspector) ProjectOf(context.Context, string) string { return "scratch" }
func (i finishingInspector) Changes(context.Context, string) (*readmodel.ChangeSummary, error) {
	close(i.entered)
	<-i.release
	return nil, nil
}

func TestChildFinishesBetweenHandlerReturnAndReplySave(t *testing.T) {
	inspector := finishingInspector{make(chan struct{}), make(chan struct{})}
	step := readmodel.FromStepProgress("#59", readmodel.Progress{Model: "child-model"}, readmodel.StepInfo{Kind: "delegate", State: "running"})
	var s *Service
	s = New(stepHandler(func(req turn.Request) turn.Result {
		s.UpdateStep(req.ConversationID, "59", step)
		return turn.Result{Text: "parent finished", Attempt: "parent-attempt"}
	}), "owner", nil)
	s.SetInspector(inspector)
	e, _, err := s.enqueue(t.Context(), "main", "delegate", "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-inspector.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not finish")
	}
	step.State, step.Answer = "done", "child finished after its parent returned"
	s.UpdateStep("main", "59", step)
	close(inspector.release)
	<-e.done
	if got := s.Replies("main")[1].Process.Steps[0]; got.State != "done" || got.Answer != step.Answer {
		t.Fatalf("completion fell into the save gap: %+v", got)
	}
}

func TestReplacementTurnKeepsSeparateChildrenWhileOldTurnFinishes(t *testing.T) {
	h := &queueHandler{started: make(chan *queueCall, 4)}
	s := New(h, "owner", nil)
	first, _, err := s.enqueue(t.Context(), "main", "first", "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	oldCall := nextCall(t, h)
	step := readmodel.FromStepProgress("#59", readmodel.Progress{}, readmodel.StepInfo{Kind: "delegate", State: "running"})
	s.UpdateStep("main", "59", step)
	next, _, err := s.enqueue(t.Context(), "main", "!replacement", "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	newCall := nextCall(t, h)
	s.UpdateStep("main", "60", step)
	step.State = "done"
	s.UpdateStep("main", "59", step)
	oldCall.finish <- nil
	<-first.done
	newCall.finish <- nil
	<-next.done
	if got := first.outcome.reply.Process.Steps; len(got) != 1 || got[0].ID != "#59" || got[0].State != "done" {
		t.Fatalf("old turn lost its child: %+v", got)
	}
	if got := next.outcome.reply.Process.Steps; len(got) != 1 || got[0].ID != "#60" {
		t.Fatalf("replacement lost or adopted another turn's child: %+v", got)
	}
}
