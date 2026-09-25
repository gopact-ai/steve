package exec

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/view"
)

// observedStep is what a step observer saw, in order.
type observedStep struct {
	mu    sync.Mutex
	calls []observedUpdate
}

type observedUpdate struct {
	p     view.Progress
	ended bool
}

func (o *observedStep) observe(_ StepRequest, p view.Progress, ended bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls = append(o.calls, observedUpdate{p, ended})
}

// requireEndedWith checks that the observer saw the step end exactly once,
// last, with the snapshot answer.
func (o *observedStep) requireEndedWith(t *testing.T, answer string) {
	t.Helper()
	o.mu.Lock()
	defer o.mu.Unlock()
	ends := 0
	for _, c := range o.calls {
		if c.ended {
			ends++
		}
	}
	n := len(o.calls)
	if ends != 1 || !o.calls[n-1].ended || o.calls[n-1].p.Answer != answer || o.calls[n-1].p.Agent != "builder" {
		t.Fatalf("observed %+v, want one end last with %q", o.calls, answer)
	}
}

// reportingSessions open a session that reports two snapshots and then
// ends its prompt the way end says.
type reportingSessions struct {
	end func(context.Context) (string, error)
}

func (s reportingSessions) OpenSession(context.Context, harness.Placement, string, string, []acp.MCPServer) (harness.Runner, error) {
	return reportingRunner(s), nil
}
func (reportingSessions) CloseSession(context.Context, harness.Placement, string) error { return nil }

type reportingRunner reportingSessions

func (reportingRunner) ID() string { return "reporting-test" }
func (r reportingRunner) Prompt(ctx context.Context, _ string, progress func(view.Progress)) (string, []string, error) {
	progress(view.Progress{Answer: "working"})
	progress(view.Progress{Answer: "working… done"})
	answer, err := r.end(ctx)
	return answer, nil, err
}
func (reportingRunner) Cancel(context.Context) error { return nil }
func (reportingRunner) Abort()                       {}

func TestAgentRunnerEndsEveryStepWithItsLastSnapshot(t *testing.T) {
	failed := errors.New("prompt failed")
	for name, end := range map[string]func(context.Context, context.CancelFunc) (string, error){
		"completed": func(context.Context, context.CancelFunc) (string, error) { return "done", nil },
		"failed":    func(context.Context, context.CancelFunc) (string, error) { return "", failed },
		"cancelled": func(ctx context.Context, cancel context.CancelFunc) (string, error) {
			cancel()
			<-ctx.Done()
			return "", ctx.Err()
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			sessions := reportingSessions{end: func(ctx context.Context) (string, error) { return end(ctx, cancel) }}
			runner := NewAgentRunner(sessions, nil, testRoster(t, bothNodes()))
			var seen observedStep
			runner.SetObserver(seen.observe)
			_, _ = runner.RunStep(ctx, StepRequest{Agent: "builder", Workspace: t.TempDir(), Goal: "work"})
			seen.requireEndedWith(t, "working… done")
		})
	}
}
