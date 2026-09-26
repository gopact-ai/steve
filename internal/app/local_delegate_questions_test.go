package app

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/console"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/delegate"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/roster"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn/turntest"
	"github.com/gopact-ai/steve/internal/view"
)

type localChildSessions struct {
	turn func(context.Context, permission.AskFunc, acphost.AskUserFunc) (string, error)
}

func (s *localChildSessions) OpenSession(context.Context, harness.Placement, string, string, []acp.MCPServer) (harness.Runner, error) {
	return &localChildRunner{turn: s.turn}, nil
}
func (*localChildSessions) CloseSession(context.Context, harness.Placement, string) error { return nil }

type localChildRunner struct {
	turn func(context.Context, permission.AskFunc, acphost.AskUserFunc) (string, error)
}

func (*localChildRunner) ID() string { return "hub-child-session" }
func (*localChildRunner) Prompt(context.Context, string, func(view.Progress)) (string, []string, error) {
	return "the owner was never reachable", nil, nil
}
func (r *localChildRunner) PromptTurn(ctx context.Context, _ string, _ []harness.Media, ask permission.AskFunc, askUser acphost.AskUserFunc, _ func(view.Progress)) (string, []string, error) {
	out, err := r.turn(ctx, ask, askUser)
	return out, nil, err
}
func (*localChildRunner) Cancel(context.Context) error { return nil }
func (*localChildRunner) Abort()                       {}

// A child running on the hub puts its permission request and its question
// in its parent's console conversation, outside the parent's exchange, and
// waits for the owner longer than its own silence limit allows.
func TestHubLocalChildWaitsForItsOwnerInTheParentConversation(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	projects := project.Open(book)
	if err := projects.Declare(t.Context(), []project.Project{{ID: "p", Home: project.Home{Path: t.TempDir()}}}); err != nil {
		t.Fatal(err)
	}
	catalog, err := agent.NewCatalog(map[string]agent.Config{"builder": {Harness: "mock", Default: true}, "helper": {Harness: "mock"}})
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := task.OpenLedger(testLedger(t))
	if err != nil {
		t.Fatal(err)
	}
	tasks.SetBudget(10, time.Hour)
	sessions := &localChildSessions{turn: func(ctx context.Context, ask permission.AskFunc, askUser acphost.AskUserFunc) (string, error) {
		if ask == nil || askUser == nil {
			return "nobody to ask", nil
		}
		outcome, err := ask(ctx, permission.Ask{SessionID: "acp-1", ToolName: "Edit", Options: []acp.PermissionOption{{OptionID: "allow", Name: "Allow", Kind: acp.PermissionOptionKindAllowOnce}, {OptionID: "reject", Name: "Reject", Kind: acp.PermissionOptionKindRejectOnce}}})
		if err != nil {
			return "", err
		}
		answer, err := askUser(ctx, view.Question{SessionID: "acp-1", Message: "Which colour?", Choices: []view.Choice{{Value: "Blue", Label: "Blue"}}})
		if err != nil {
			return "", err
		}
		return "permission:" + string(outcome.OptionID) + " accept:" + answer.Value, nil
	}}
	artifacts := artifact.New(filepath.Join(t.TempDir(), "artifacts"), book, projects, artifact.LocalNodes{Dir: t.TempDir()})
	delegation := delegate.New(tasks, roster.New(catalog), sessions, capability.NewAssembler(nil), artifacts, "hub", i18n.New(i18n.LocaleZH))
	delegation.SetLedger(attempt.New(book), artifacts)
	// Long enough that starting and finishing the child under load is
	// not mistaken for silence; the waits below still exceed it.
	delegation.MaxSilence = time.Second
	delegation.InlineWait = 0
	cons := console.New(turntest.IdleCoordinator{}, "owner", nil)
	wireDelegateQuestions(delegation, cons)

	conversation := "console:parent"
	parent, err := tasks.Create(task.Task{Goal: "the big goal", Channel: conversation, Transport: "console", AnchorMessage: console.AnchorMark + "parent-exchange", Member: "builder", Node: "hub", ProjectID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Begin(parent.ID, "builder", "hub", ""); err != nil {
		t.Fatal(err)
	}
	started, err := delegation.Start(t.Context(), conversation, "builder", agentmcp.DelegateRequest{Goal: "askme", Agent: "helper"})
	if err != nil {
		t.Fatal(err)
	}
	answers := []consoleapi.QuestionAnswer{{CommandID: "grant", Decision: "accept", Choice: "allow"}, {CommandID: "colour", Decision: "accept", Choice: "Blue"}}
	for _, answer := range answers {
		q := awaitLocalChildQuestion(t, cons, conversation)
		if q.ExchangeID != "" || q.ParentTaskID != parent.ID || q.TaskID != started.TaskID || q.AttemptID == "" || q.SessionID != "hub-child-session" || q.Project != "p" || !q.Deadline.IsZero() {
			t.Fatalf("child question lost its own execution: %+v", q)
		}
		// Longer than the child's whole silence limit: a person thinking
		// is not the agent hanging.
		time.Sleep(delegation.MaxSilence * 3 / 2)
		if _, err := cons.AnswerQuestion(t.Context(), q.ID, answer); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		child, _ := tasks.Get(started.TaskID)
		if child.Result != nil && child.State.Terminal() {
			if child.State != task.StateDone || !strings.Contains(child.Result.Answer, "permission:allow accept:Blue") {
				t.Fatalf("child did not receive its answers: %s %+v", child.State, child.Result)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("child never finished: %+v", child)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func awaitLocalChildQuestion(t *testing.T, cons *console.Service, conversation string) consoleapi.PendingQuestion {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		for _, q := range cons.Questions(conversation) {
			if q.State == "pending" {
				return q
			}
		}
	}
	t.Fatalf("no pending question in %s: %+v", conversation, cons.Questions(""))
	return consoleapi.PendingQuestion{}
}
