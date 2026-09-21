package console

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/turn"
)

type scheduleSubmissionHandler struct {
	queueHandler
	project string
}

func (h *scheduleSubmissionHandler) Context(context.Context, string) (turn.Context, error) {
	return turn.Context{Project: &turn.ContextProject{ID: h.project, Bound: true}}, nil
}

func scheduleSubmission(t *testing.T, id, project string) consoleapi.Submission {
	t.Helper()
	raw, err := json.Marshal(map[string]string{
		"conversation": "console:work", "input": "@builder /every 30m report",
		"command_id": id, "expected_project": project,
	})
	if err != nil {
		t.Fatal(err)
	}
	var req consoleapi.Submission
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}
	return req
}

func TestScheduleSubmissionRejectsProjectDriftBeforeAcceptance(t *testing.T) {
	h := &scheduleSubmissionHandler{queueHandler: queueHandler{started: make(chan *queueCall, 8)}, project: "q"}
	s := New(h, "owner", nil)
	_, err := s.Submit(t.Context(), scheduleSubmission(t, "not-yet-sent", "p"))
	if err == nil {
		t.Fatal("first submission followed the conversation from p to q")
	}
	if len(s.Queue("work")) != 0 {
		t.Fatal("drifted submission was accepted")
	}
	noCall(t, &h.queueHandler)
}

func TestScheduleSubmissionProjectIsBoundToReceipt(t *testing.T) {
	h := &scheduleSubmissionHandler{queueHandler: queueHandler{started: make(chan *queueCall, 8)}, project: "p"}
	s := New(h, "owner", nil)
	doc := &memDoc{}
	if err := s.Persist(doc); err != nil {
		t.Fatal(err)
	}
	req := scheduleSubmission(t, "same", "p")
	e, err := s.Submit(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	call := nextCall(t, &h.queueHandler)
	call.finish <- nil
	awaitExchange(t, s, e.ID)
	h.project = "q"
	// An accepted request replays even when the current binding has changed.
	reply, err := s.SendSubmission(t.Context(), req)
	if err != nil || reply.ExchangeID != e.ID || reply.ProjectID != "p" {
		t.Fatalf("lost original receipt: %+v %v", reply, err)
	}
	for _, project := range []string{"q", ""} {
		if _, err := s.Submit(t.Context(), scheduleSubmission(t, "same", project)); !errors.Is(err, consoleapi.ErrCommandConflict) {
			t.Errorf("same ID with expected project %q: %v", project, err)
		}
	}
	raw, _, _ := doc.Load()
	restored := New(h, "owner", nil)
	if err := restored.Persist(&memDoc{raw: raw, saved: true}); err != nil {
		t.Fatal(err)
	}
	replayed, err := restored.SendSubmission(t.Context(), req)
	if err != nil || replayed.ID != reply.ID {
		t.Fatalf("persisted receipt changed: %+v %v", replayed, err)
	}
	noCall(t, &h.queueHandler)
}
