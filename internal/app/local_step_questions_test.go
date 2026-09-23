package app

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
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
	conversation := "console:plan"
	tasks, attempts, tracked, record, scope := localStepFixture(t, task.Task{Goal: "plan", Channel: conversation, Transport: "console", AnchorMessage: console.AnchorMark + "plan-exchange"})
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
	// A read-only probe names an execution but runs none.
	probe := execution.WithProbeKey(ctx, execution.Key{TaskID: tracked.ID, InstanceID: "plan/s1", AttemptID: record.ID})
	if _, err := askUser(probe, view.Question{SessionID: "acp-step", Message: "?", AllowFreeText: true}); err == nil {
		t.Fatal("a probe asked on behalf of an execution")
	}
	// Only plan work asks through this path; a turn has its own.
	chat, err := attempts.Open(t.Context(), attempt.Spec{TaskID: tracked.ID, TurnID: "m1", Kind: attempt.KindChat, Project: "p", Harness: "mock", Agent: "worker", Scope: attempt.ScopeNone, By: "test"})
	if err != nil {
		t.Fatal(err)
	}
	chatScope, err := execution.New(t.Context(), tasks).Begin(t.Context(), execution.Key{TaskID: tracked.ID, InstanceID: "chat", AttemptID: chat.ID})
	if err != nil {
		t.Fatal(err)
	}
	defer chatScope.Finish(nil)
	chatCtx, chatCancel := context.WithTimeout(chatScope.Context(), time.Second)
	defer chatCancel()
	if _, err := askUser(chatCtx, view.Question{SessionID: "acp-step", Message: "?", AllowFreeText: true}); err == nil {
		t.Fatal("a chat attempt asked through the plan path")
	}
}

type localStepScope interface{ Context() context.Context }

// localStepFixture runs one hub-local plan step of a task opened from
// origin, inside the execution scope the hub gives it.
func localStepFixture(t *testing.T, origin task.Task) (*task.Store, *attempt.Service, task.Task, attempt.Record, localStepScope) {
	t.Helper()
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
	origin.Member, origin.Node, origin.ProjectID, origin.Origin = "worker", "hub", "p", "plan"
	tracked, err := tasks.Create(origin)
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
	t.Cleanup(func() { scope.Finish(nil) })
	return tasks, attempts, tracked, record, scope
}

// A step of a task from another channel asks in a recovery conversation
// that names where the task came from.
func TestHubLocalStepFromAnotherChannelAsksInANamedRecoveryConversation(t *testing.T) {
	tasks, attempts, tracked, _, scope := localStepFixture(t, task.Task{Goal: "plan", Channel: "stdio:session", Transport: "stdio"})
	cons := console.New(nil, "owner", nil)
	_, askUser := planQuestions(cons, tasks, attempts)
	go askUser(scope.Context(), view.Question{SessionID: "acp-step", Message: "Which colour?", AllowFreeText: true})
	q := awaitLocalChildQuestion(t, cons, "console:recovery:"+tracked.ID)
	for _, summary := range cons.Summaries(t.Context()) {
		if summary.ID == q.Conversation && (strings.Contains(summary.Title, "飞书") || !strings.Contains(summary.Title, "stdio")) {
			t.Fatalf("recovery conversation misnames its source: %q", summary.Title)
		}
	}
}
