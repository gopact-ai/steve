package exec

import (
	"context"
	"testing"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/view"
)

type askingSessions struct{}

func (askingSessions) OpenSession(context.Context, harness.Placement, string, string, []acp.MCPServer) (harness.Runner, error) {
	return askingSession{}, nil
}
func (askingSessions) CloseSession(context.Context, harness.Placement, string) error { return nil }

type askingSession struct{}

func (askingSession) ID() string { return "hub-step-session" }
func (askingSession) Prompt(context.Context, string, func(view.Progress)) (string, []string, error) {
	return "nobody to ask", nil, nil
}
func (askingSession) PromptTurn(ctx context.Context, _ string, _ []harness.Media, _ permission.AskFunc, askUser acphost.AskUserFunc, _ func(view.Progress)) (string, []string, error) {
	if askUser == nil {
		return "nobody to ask", nil, nil
	}
	answer, err := askUser(ctx, view.Question{Message: "Which colour?"})
	return "accept:" + answer.Value, nil, err
}
func (askingSession) Cancel(context.Context) error { return nil }
func (askingSession) Abort()                       {}

// A step on the hub reaches whoever answers the plan's questions, as a
// node-owned step does.
func TestHubLocalStepReachesItsQuestionHandlers(t *testing.T) {
	runner := NewAgentRunner(askingSessions{}, nil, testRoster(t, bothNodes()))
	runner.SetQuestionHandlers(nil, func(context.Context, view.Question) (view.Answer, error) {
		return view.Answer{Value: "Blue"}, nil
	})
	result, err := runner.RunStep(t.Context(), StepRequest{Agent: "local", TaskID: "task", PlanID: "plan", StepID: "step"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != "accept:Blue" {
		t.Fatalf("hub-local step never reached its owner: %q", result.Answer)
	}
}
