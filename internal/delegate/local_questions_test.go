package delegate

import (
	"context"
	"strings"
	"testing"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/view"
)

// A child running on the hub itself asks its owner through the same
// handlers as a node-owned child, bound to its own execution, and the
// answer reaches the child's turn.
func TestHubLocalChildAsksItsOwnerThroughItsOwnExecution(t *testing.T) {
	w := newWorld(t)
	parent := w.running(t, "builder")
	var questions, permissions []QuestionBinding
	w.service.SetQuestionHandlers(
		func(_ context.Context, binding QuestionBinding, ask permission.Ask) (acp.RequestPermissionOutcome, error) {
			permissions = append(permissions, binding)
			return acp.SelectedRequestPermissionOutcome(ask.Options[0].OptionID), nil
		},
		func(_ context.Context, binding QuestionBinding, q view.Question) (view.Answer, error) {
			questions = append(questions, binding)
			return view.Answer{Value: "Blue"}, nil
		})
	w.sessions.turn = func(ctx context.Context, ask permission.AskFunc, askUser acphost.AskUserFunc) (string, error) {
		if ask == nil || askUser == nil {
			return "nobody to ask", nil
		}
		outcome, err := ask(ctx, permission.Ask{ToolName: "Edit", Options: []acp.PermissionOption{{OptionID: "allow", Kind: acp.PermissionOptionKindAllowOnce}}})
		if err != nil {
			return "", err
		}
		answer, err := askUser(ctx, view.Question{Message: "Which colour?", Choices: []view.Choice{{Value: "Blue"}}})
		if err != nil {
			return "", err
		}
		return "permission:" + string(outcome.OptionID) + " accept:" + answer.Value, nil
	}
	result, err := w.service.Delegate(t.Context(), "chat", "builder", agentmcp.DelegateRequest{Goal: "askme", Agent: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Node != "" || !strings.Contains(result.Answer, "permission:allow accept:Blue") {
		t.Fatalf("hub-local child did not reach its owner: %+v", result)
	}
	child, _ := w.tasks.Get(result.TaskID)
	if child.State != task.StateDone {
		t.Fatalf("child = %+v", child)
	}
	if len(questions) != 1 || len(permissions) != 1 {
		t.Fatalf("questions=%d permissions=%d", len(questions), len(permissions))
	}
	for _, binding := range []QuestionBinding{questions[0], permissions[0]} {
		if binding.Conversation != "chat" || binding.ParentTask != parent.ID || binding.Task != result.TaskID || binding.Attempt == "" || binding.Session != "child-session" || binding.Agent != "codex" || binding.Project != "p" {
			t.Fatalf("question bound to the wrong execution: %+v", binding)
		}
	}
}
