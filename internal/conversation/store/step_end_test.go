package store

import (
	"github.com/gopact-ai/steve/internal/conversationapi"
	"github.com/gopact-ai/steve/internal/view"
	"testing"
)

// History is synchronous and remains complete with no renderer or event
// subscription, even when a newer turn is finishing at the same time.
func TestExecutionHistoryKeepsFinalStepWithoutRenderer(t *testing.T) {
	h := &queueHandler{started: make(chan *queueCall, 4)}
	s := New(h, "owner", nil)
	first, _, err := s.enqueue(t.Context(), "main", "first", nil, enqueueOptions{})
	if err != nil {
		t.Fatal(err)
	}
	old := nextCall(t, h)
	second, _, err := s.enqueue(t.Context(), "main", "!replacement", nil, enqueueOptions{})
	if err != nil {
		t.Fatal(err)
	}
	newer := nextCall(t, h)
	for _, reasoning := range []string{"compiling", "compiling… done"} {
		s.UpdateExecutionStep("console:main", AnchorMark+first.ID, "task", conversationapi.FromStepProgress("plan/build", conversationapi.FromProgress(view.Progress{Reasoning: reasoning}), conversationapi.StepInfo{}))
	}
	old.finish <- nil
	<-first.done
	// The first snapshot of a late child still goes to the original reply;
	// it does not need to have appeared before the parent completed.
	s.UpdateExecutionStep("console:main", AnchorMark+first.ID, "child", conversationapi.StepProcess{ID: "#child", StepInfo: conversationapi.StepInfo{State: "done", Answer: "late final"}})
	s.UpdateExecutionStep("console:main", AnchorMark+"missing", "child", conversationapi.StepProcess{ID: "#unknown"})
	newer.finish <- nil
	<-second.done
	replies := s.Replies("main")
	var history *conversationapi.Process
	for _, r := range replies {
		if r.ExchangeID == first.ID && r.Kind == "reply" {
			history = r.Process
		}
	}
	if history == nil || len(history.Steps) != 2 || history.Steps[0].Reasoning != "compiling… done" || history.Steps[1].Answer != "late final" {
		t.Fatalf("incomplete original history: %+v", history)
	}
	if got := second.outcome.reply.Process; got != nil && len(got.Steps) != 0 {
		t.Fatalf("replacement adopted original/missing step: %+v", got)
	}
}
