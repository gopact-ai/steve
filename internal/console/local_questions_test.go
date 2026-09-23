package console

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/idle"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/view"
)

func localQuestionBase() consoleapi.PendingQuestion {
	return consoleapi.PendingQuestion{Conversation: "console:parent", Project: "p", TaskID: "child-task", AttemptID: "child-attempt", SessionID: "local-session"}
}

// A hub-local child asks in its parent's conversation, bound to its own
// task and attempt rather than to the parent's exchange, so the parent's
// turn ending does not retire it; it waits for the owner without a
// deadline and without the child's silence clock running out meanwhile.
func TestLocalChildQuestionWaitsInParentConversationBeyondSilence(t *testing.T) {
	service := New(&echo{}, "owner", nil)
	clock, stop, _ := idle.WithTimeout(t.Context(), 40*time.Millisecond)
	defer stop()
	// Paused until the question is up, so a slow start is not what is
	// measured; resuming does not restart a clock the question holds.
	clock.Pause()
	type result struct {
		answer view.Answer
		err    error
	}
	done := make(chan result, 1)
	go func() {
		answer, err := service.RequestLocalQuestion(clock, localQuestionBase(), view.Question{SessionID: "acp-session", Message: "Which branch?", AllowFreeText: true})
		done <- result{answer, err}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	q := pendingNativeQuestionForTest(t, ctx, service, make(chan error))
	if q.Conversation != "console:parent" || q.ExchangeID != "" || q.TaskID != "child-task" || q.AttemptID != "child-attempt" || q.SessionID != "local-session" || q.Principal != "owner" || !q.Deadline.IsZero() {
		t.Fatalf("local child question lost its binding: %+v", q)
	}
	clock.Resume()
	time.Sleep(100 * time.Millisecond)
	if clock.Err() != nil {
		t.Fatalf("waiting on the owner counted as the child's silence: %v", clock.Err())
	}
	if _, err := service.AnswerQuestion(ctx, q.ID, consoleapi.QuestionAnswer{CommandID: "c1", Decision: "accept", Text: "main"}); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.err != nil || got.answer.Text != "main" {
		t.Fatalf("answer = %+v, %v", got.answer, got.err)
	}
}

func TestLocalChildPermissionReturnsTheChosenOption(t *testing.T) {
	service := New(&echo{}, "owner", nil)
	done := make(chan acp.RequestPermissionOutcome, 1)
	ask := permission.Ask{SessionID: "acp-session", ToolName: "Edit", Options: []acp.PermissionOption{{OptionID: "allow", Name: "Allow", Kind: acp.PermissionOptionKindAllowOnce}, {OptionID: "reject", Name: "Reject", Kind: acp.PermissionOptionKindRejectOnce}}}
	go func() {
		outcome, _ := service.RequestLocalPermission(t.Context(), localQuestionBase(), ask)
		done <- outcome
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	q := pendingNativeQuestionForTest(t, ctx, service, make(chan error))
	if q.Kind != "permission" || q.TaskID != "child-task" || !q.Deadline.IsZero() {
		t.Fatalf("local permission lost its binding: %+v", q)
	}
	if _, err := service.AnswerQuestion(ctx, q.ID, consoleapi.QuestionAnswer{CommandID: "c1", Decision: "accept", Choice: "allow"}); err != nil {
		t.Fatal(err)
	}
	if outcome := <-done; outcome.Outcome != acp.RequestPermissionOutcomeTypeSelected || outcome.OptionID != "allow" {
		t.Fatalf("outcome = %+v", outcome)
	}
}

// The local entry trusts the hub's own execution record. A node-owned
// session, an exchange binding or a missing execution
// must never reach the owner through it.
func TestLocalChildQuestionRejectsBindingsItDoesNotOwn(t *testing.T) {
	service := New(&echo{}, "owner", nil)
	cases := map[string]func() (context.Context, consoleapi.PendingQuestion){
		"node session": func() (context.Context, consoleapi.PendingQuestion) {
			b := localQuestionBase()
			b.SessionID = "ns_x"
			return t.Context(), b
		},
		"exchange": func() (context.Context, consoleapi.PendingQuestion) {
			b := localQuestionBase()
			b.ExchangeID = "e1"
			return t.Context(), b
		},
		"no attempt": func() (context.Context, consoleapi.PendingQuestion) {
			b := localQuestionBase()
			b.AttemptID = ""
			return t.Context(), b
		},
		"no task": func() (context.Context, consoleapi.PendingQuestion) {
			b := localQuestionBase()
			b.TaskID = ""
			return t.Context(), b
		},
		"no session": func() (context.Context, consoleapi.PendingQuestion) {
			b := localQuestionBase()
			b.SessionID = ""
			return t.Context(), b
		},
		"no conversation": func() (context.Context, consoleapi.PendingQuestion) {
			b := localQuestionBase()
			b.Conversation = ""
			return t.Context(), b
		},
	}
	for name, build := range cases {
		ctx, binding := build()
		if _, err := service.RequestLocalQuestion(ctx, binding, view.Question{Message: "?", AllowFreeText: true}); !errors.Is(err, consoleapi.ErrInvalidAnswer) {
			t.Errorf("%s: question accepted: %v", name, err)
		}
		if _, err := service.RequestLocalPermission(ctx, binding, permission.Ask{Options: []acp.PermissionOption{{OptionID: "a", Kind: acp.PermissionOptionKindAllowOnce}}}); !errors.Is(err, consoleapi.ErrInvalidAnswer) {
			t.Errorf("%s: permission accepted: %v", name, err)
		}
	}
	if len(service.Questions("")) != 0 {
		t.Fatal("a rejected binding reached the owner")
	}
}
