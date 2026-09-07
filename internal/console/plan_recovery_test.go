package console

import (
	"context"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/turn"
	"github.com/gopact-ai/steve/internal/view"
)

type planRecoveryDriver struct {
	recoveryDriver
	plans      []turn.RetainedPlan
	resumePlan func(context.Context, turn.RetainedPlan, turn.Request) (turn.Result, error)
}

func (d *planRecoveryDriver) RetainedPlans(context.Context) ([]turn.RetainedPlan, error) {
	return d.plans, nil
}
func (d *planRecoveryDriver) ResumeRetainedPlan(ctx context.Context, p turn.RetainedPlan, req turn.Request) (turn.Result, error) {
	return d.resumePlan(ctx, p, req)
}
func (d *planRecoveryDriver) PlanRelocation(context.Context, string, turn.Request) (turn.RelocationPlan, error) {
	panic("plan was sent through chat relocation")
}
func (d *planRecoveryDriver) RelocateChat(context.Context, string, string, turn.Request) (turn.Result, error) {
	panic("plan was sent through chat replacement")
}

func TestPlanRecoveryUsesOriginalExchangeAndNativeQuestionCallbacks(t *testing.T) {
	lifetime, cancel := context.WithCancel(t.Context())
	defer cancel()
	s := New(&echo{}, "owner", nil)
	s.EnableRetainedRecovery(lifetime)
	if err := s.Persist(recoveryDocument()); err != nil {
		t.Fatal(err)
	}
	driver := &planRecoveryDriver{recoveryDriver: recoveryDriver{candidates: []turn.RetainedChat{}}, plans: []turn.RetainedPlan{{TaskID: "plan-task", Conversation: "console:main", MessageID: "web-e1", PlanID: "plan-1", RunID: "run-1", AttemptID: "planning-attempt", ProjectID: "p"}}}
	driver.resumePlan = func(ctx context.Context, p turn.RetainedPlan, req turn.Request) (turn.Result, error) {
		if p.PlanID != "plan-1" || p.RunID != "run-1" || req.MessageID != "web-e1" {
			t.Error("plan identity changed")
		}
		req.OnTurnReady(p.TaskID, "retained-step")
		answer, err := req.OnAskUser(ctx, view.Question{SessionID: "ns_original_step", RequestID: "nq_original_question", Message: "Continue the original step?", Required: true, Choices: []view.Choice{{Value: "yes", Label: "Continue"}}})
		if err != nil {
			return turn.Result{}, err
		}
		if answer.Value != "yes" {
			t.Error("saved step question answer not delivered")
		}
		return turn.Result{Text: "completed original plan"}, nil
	}
	if err := s.RecoverChats(lifetime, driver); err != nil {
		t.Fatal(err)
	}
	var pending consoleapi.PendingQuestion
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		list := s.Questions("main")
		if len(list) > 0 {
			pending = list[0]
			break
		}
		time.Sleep(time.Millisecond)
	}
	if pending.TaskID != "plan-task" || pending.AttemptID != "retained-step" || pending.ExchangeID != "e1" || pending.SessionID != "ns_original_step" {
		t.Fatalf("plan callback lost original binding: %+v", pending)
	}
	if _, err := s.AnswerQuestion(t.Context(), pending.ID, consoleapi.QuestionAnswer{CommandID: "resume-step", Decision: "accept", Choice: "yes"}); err != nil {
		t.Fatal(err)
	}
	if e := awaitExchange(t, s, "e1"); e.State != "done" {
		t.Fatalf("plan recovery did not complete original exchange: %+v", e)
	}
	if driver.calls.Load() != 0 {
		t.Fatal("plan invoked chat recovery")
	}
	cancel()
	if err := s.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestDetachedPlanSelectsPlanRecoveryInsteadOfRepeatingSubmission(t *testing.T) {
	h := &queueHandler{started: make(chan *queueCall, 1)}
	s := New(h, "owner", nil)
	s.EnableRetainedRecovery(t.Context())
	if err := s.Persist(&memDoc{}); err != nil {
		t.Fatal(err)
	}
	driver := &planRecoveryDriver{recoveryDriver: recoveryDriver{candidates: []turn.RetainedChat{}}, resumePlan: func(_ context.Context, p turn.RetainedPlan, _ turn.Request) (turn.Result, error) {
		return turn.Result{Text: "original plan finished"}, nil
	}}
	if err := s.RecoverChats(t.Context(), driver); err != nil {
		t.Fatal(err)
	}
	e := enqueueForTest(t, s, "main", "/plan original goal")
	call := nextCall(t, h)
	driver.plans = []turn.RetainedPlan{{TaskID: "plan-task", Conversation: e.Conversation, MessageID: AnchorMark + e.ID, PlanID: "plan-1", RunID: "run-1"}}
	call.finish <- harness.ErrStopUnconfirmed
	if got := awaitExchange(t, s, e.ID); got.State != "done" {
		t.Fatalf("plan observer ended original exchange: %+v", got)
	}
	noCall(t, h)
}
