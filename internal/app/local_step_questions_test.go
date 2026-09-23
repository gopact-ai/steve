package app

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/console"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/view"
)

// A plan step or auxiliary execution running on the hub asks from inside
// its own execution scope; the question lands in its task's conversation,
// bound to that attempt rather than to the exchange that started the plan.
func TestHubLocalStepAsksFromItsOwnExecution(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	tasks, err := task.Open(filepath.Join(t.TempDir(), "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	attempts := attempt.New(book)
	conversation := "console:plan"
	tracked, err := tasks.Create(task.Task{Goal: "plan", Channel: conversation, Transport: "console", AnchorMessage: console.AnchorMark + "plan-exchange", Member: "worker", Node: "hub", ProjectID: "p", Origin: "plan"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Begin(tracked.ID, "worker", "hub", ""); err != nil {
		t.Fatal(err)
	}
	record, err := attempts.Open(t.Context(), attempt.Spec{TaskID: tracked.ID, TurnID: "plan/s1", Kind: attempt.KindStep, Project: "p", Harness: "mock", Agent: "worker", Scope: attempt.ScopeNone, By: "test"})
	if err != nil {
		t.Fatal(err)
	}
	scope, err := execution.New(t.Context(), tasks).Begin(t.Context(), execution.Key{TaskID: tracked.ID, InstanceID: "plan/s1", AttemptID: record.ID})
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Finish(nil)
	cons := console.New(nil, "owner", nil)
	ask, askUser := planQuestions(cons, tasks, attempts)

	answered := make(chan view.Answer, 1)
	go func() {
		answer, _ := askUser(scope.Context(), view.Question{SessionID: "acp-step", Message: "Which colour?", AllowFreeText: true})
		answered <- answer
	}()
	q := awaitLocalChildQuestion(t, cons, conversation)
	if q.ExchangeID != "" || q.TaskID != tracked.ID || q.AttemptID != record.ID || q.SessionID != "acp-step" || q.Project != "p" || !q.Deadline.IsZero() {
		t.Fatalf("step question lost its execution: %+v", q)
	}
	if _, err := cons.AnswerQuestion(t.Context(), q.ID, consoleapi.QuestionAnswer{CommandID: "c1", Decision: "accept", Text: "Blue"}); err != nil {
		t.Fatal(err)
	}
	if got := <-answered; got.Text != "Blue" {
		t.Fatalf("answer = %+v", got)
	}

	granted := make(chan acp.RequestPermissionOutcome, 1)
	go func() {
		outcome, _ := ask(scope.Context(), permission.Ask{SessionID: "acp-step", ToolName: "Edit", Options: []acp.PermissionOption{{OptionID: "allow", Kind: acp.PermissionOptionKindAllowOnce}}})
		granted <- outcome
	}()
	q = awaitLocalChildQuestion(t, cons, conversation)
	if q.Kind != "permission" || q.AttemptID != record.ID {
		t.Fatalf("step permission lost its execution: %+v", q)
	}
	if _, err := cons.AnswerQuestion(t.Context(), q.ID, consoleapi.QuestionAnswer{CommandID: "c2", Decision: "accept", Choice: "allow"}); err != nil {
		t.Fatal(err)
	}
	if got := <-granted; got.OptionID != "allow" {
		t.Fatalf("outcome = %+v", got)
	}

	// Outside an execution scope nobody vouches for the asker.
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := askUser(ctx, view.Question{SessionID: "acp-step", Message: "?", AllowFreeText: true}); !errors.Is(err, consoleapi.ErrInvalidAnswer) {
		t.Fatalf("unscoped question accepted: %v", err)
	}
}
