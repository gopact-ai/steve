package console

import (
	"context"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/schedule"
)

type scheduledInspector struct{ project string }

func (i *scheduledInspector) ProjectOf(context.Context, string) string { return i.project }
func (*scheduledInspector) Changes(context.Context, string) (*consoleapi.ChangeSummary, error) {
	return nil, nil
}

func TestScheduledExchangePersistsOriginalContextAndDeduplicates(t *testing.T) {
	h := &queueHandler{started: make(chan *queueCall, 8)}
	s := New(h, "owner", nil)
	doc := &memDoc{}
	if err := s.Persist(doc); err != nil {
		t.Fatal(err)
	}
	i := &scheduledInspector{project: "p"}
	s.SetInspector(i)
	first := enqueueForTest(t, s, "work", "block")
	one := nextCall(t, h)
	f := schedule.Firing{Job: schedule.Job{ID: "1", Channel: "console", ConversationID: "console:work", ProjectID: "p", Requester: "owner", Member: "builder", Prompt: "scheduled work"}, Key: "schedule:1:fixed-time", ScheduledAt: time.Now()}
	e, err := s.EnqueueScheduled(t.Context(), f)
	if err != nil {
		t.Fatal(err)
	}
	i.project = "different"
	again, err := s.EnqueueScheduled(t.Context(), f)
	if err != nil || again.ID != e.ID {
		t.Fatalf("replay lost its already accepted receipt: %+v %v", again, err)
	}
	raw, _, _ := doc.Load()
	one.finish <- nil
	awaitExchange(t, s, first.ID)
	two := nextCall(t, h)
	two.finish <- nil
	awaitExchange(t, s, e.ID)
	h2 := &queueHandler{started: make(chan *queueCall, 8)}
	restored := New(h2, "owner", nil)
	restored.SetInspector(&scheduledInspector{project: "different"})
	if err := restored.Persist(&memDoc{raw: raw, saved: true}); err != nil {
		t.Fatal(err)
	}
	again, err = restored.EnqueueScheduled(t.Context(), f)
	if err != nil || again.ID != e.ID {
		t.Fatalf("restart lost submission: %+v %v", again, err)
	}
	if err := restored.Drain(); err != nil {
		t.Fatal(err)
	}
	call := nextCall(t, h2)
	if call.req.Channel != "console" || call.req.ExpectedProject != "p" || call.req.SenderOpenID != "owner" || call.req.Origin != "schedule:1" || call.req.Input != "@builder scheduled work" {
		t.Fatalf("schedule metadata not persisted: %+v", call.req)
	}
	call.finish <- nil
	awaitExchange(t, restored, e.ID)
	noCall(t, h2)
	newFiring := f
	newFiring.Key = "schedule:1:next-time"
	if _, err := restored.EnqueueScheduled(t.Context(), newFiring); err == nil {
		t.Fatal("new firing silently followed new project")
	}
}
