package exec

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/agentexec"
	"github.com/gopact-ai/steve/internal/budget"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/view"
)

type timeoutSessions struct {
	observe func(context.Context, time.Duration) error
}

func (s timeoutSessions) OpenSession(ctx context.Context, _ harness.Placement, _, _ string, _ []acp.MCPServer) (harness.Runner, error) {
	return timeoutSession{s: s, opened: ctx}, nil
}
func (timeoutSessions) CloseSession(context.Context, harness.Placement, string) error { return nil }

type timeoutSession struct {
	s      timeoutSessions
	opened context.Context
}

func (timeoutSession) ID() string { return "timeout-test" }
func (s timeoutSession) Prompt(ctx context.Context, _ string, _ func(view.Progress)) (string, []string, error) {
	opened, _ := s.opened.Deadline()
	deadline, _ := ctx.Deadline()
	if !opened.Equal(deadline) {
		return "", nil, fmt.Errorf("open and prompt deadlines differ")
	}
	return "done", nil, s.s.observe(ctx, 0)
}
func (timeoutSession) Cancel(context.Context) error { return nil }
func (timeoutSession) Abort()                       {}

type timeoutCommand struct {
	observe func(context.Context, time.Duration) error
}

func (c timeoutCommand) Exec(ctx context.Context, _, _, _ string) (string, error) {
	return "", c.observe(ctx, 0)
}

type timeoutVerifier struct {
	observe func(context.Context, time.Duration) error
}

func (v timeoutVerifier) Prompt(ctx context.Context, spec agentexec.Spec, _ string, validate func(string) error) (agentexec.Result, error) {
	// Match AgentVerifier's deadline ownership for this non-retained stub.
	// Retained/new production Runner deadlines are exercised separately.
	ctx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()
	if err := v.observe(ctx, spec.Timeout); err != nil {
		return agentexec.Result{}, err
	}
	return agentexec.Result{Answer: "PASS"}, validate("PASS")
}

// Each adapter goes through the public entry point, not a timeout getter.
func timeoutConsumer(t *testing.T, kind string, fallback time.Duration, source func() time.Duration, observe func(context.Context, time.Duration) error) func(context.Context) error {
	t.Helper()
	req := StepRequest{Agent: "builder", TaskID: "task", PlanID: "plan", StepID: "step"}
	if kind == "step" {
		runner := NewAgentRunner(timeoutSessions{observe}, nil, testRoster(t, bothNodes()))
		runner.Timeout, runner.TimeoutSource = fallback, source
		return func(ctx context.Context) error { _, err := runner.RunStep(ctx, req); return err }
	}
	verifier := NewVerifiers(timeoutCommand{observe}, timeoutVerifier{observe})
	verifier.Timeout, verifier.TimeoutSource = fallback, source
	check := plan.Verify{Kind: plan.VerifyCommand, Command: "check"}
	if kind == "agent" {
		check = plan.Verify{Kind: plan.VerifyAgent, Agent: "reviewer"}
	}
	return func(ctx context.Context) error { return verifier.Verify(ctx, req, check, plan.StepResult{}) }
}

// consumerTimeoutSlack is how long a consumer may take to be observed
// after its deadline is set. The policies under test differ by minutes, so
// a loaded test run cannot blur one into another.
const consumerTimeoutSlack = 10 * time.Second

func checkConsumerTimeout(ctx context.Context, spec, want time.Duration, agent bool) error {
	deadline, ok := ctx.Deadline()
	remaining := time.Until(deadline)
	if !ok || remaining > want || remaining < want-consumerTimeoutSlack {
		return fmt.Errorf("remaining=%s want=%s (deadline=%v)", remaining, want, ok)
	}
	if agent && spec != want {
		return fmt.Errorf("spec timeout=%s want=%s", spec, want)
	}
	return nil
}

func TestTimeoutSourcePinsRunningConsumersAndRefreshesRequests(t *testing.T) {
	for _, kind := range []string{"step", "command", "agent"} {
		t.Run(kind, func(t *testing.T) {
			var current atomic.Int64
			current.Store(int64(time.Minute))
			var calls atomic.Int64
			source := func() time.Duration { calls.Add(1); return time.Duration(current.Load()) }
			var entered atomic.Int64
			run := timeoutConsumer(t, kind, 9*time.Minute, source, func(ctx context.Context, spec time.Duration) error {
				want := 2 * time.Minute
				if entered.Add(1) == 1 {
					want = time.Minute
				}
				if err := checkConsumerTimeout(ctx, spec, want, kind == "agent"); err != nil {
					return err
				}
				before, _ := ctx.Deadline()
				current.Store(int64(2 * time.Minute))
				after, _ := ctx.Deadline()
				if !before.Equal(after) || ctx.Err() != nil {
					return fmt.Errorf("running request deadline changed")
				}
				return nil
			})
			for range 2 {
				if err := run(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			if calls.Load() != 2 {
				t.Fatalf("source called %d times for 2 requests", calls.Load())
			}
		})
	}
}

func TestTimeoutSourceDefaultsAndLegacyFields(t *testing.T) {
	for _, kind := range []string{"step", "command", "agent"} {
		t.Run(kind, func(t *testing.T) {
			defaultTimeout := budget.VerifyTimeout
			if kind == "step" {
				defaultTimeout = budget.StepTimeout
			}
			for _, tc := range []struct {
				name           string
				fallback, want time.Duration
				source         func() time.Duration
			}{
				{"legacy", time.Minute, time.Minute, nil},
				{"default", 0, defaultTimeout, nil},
				{"source-zero", time.Minute, defaultTimeout, func() time.Duration { return 0 }},
			} {
				t.Run(tc.name, func(t *testing.T) {
					run := timeoutConsumer(t, kind, tc.fallback, tc.source, func(ctx context.Context, spec time.Duration) error {
						return checkConsumerTimeout(ctx, spec, tc.want, kind == "agent")
					})
					if err := run(t.Context()); err != nil {
						t.Fatal(err)
					}
				})
			}
		})
	}
}

func TestTimeoutSourceConcurrentConsumers(t *testing.T) {
	for _, kind := range []string{"step", "command", "agent"} {
		t.Run(kind, func(t *testing.T) {
			var current atomic.Int64
			current.Store(int64(time.Minute))
			source := func() time.Duration { return time.Duration(current.Load()) }
			run := timeoutConsumer(t, kind, 9*time.Minute, source, func(ctx context.Context, spec time.Duration) error {
				deadline, ok := ctx.Deadline()
				if !ok {
					return fmt.Errorf("missing deadline")
				}
				want := time.Minute
				if time.Until(deadline) > time.Minute {
					want = 2 * time.Minute
				}
				return checkConsumerTimeout(ctx, spec, want, kind == "agent")
			})
			stop := make(chan struct{})
			var writer sync.WaitGroup
			writer.Go(func() {
				for i := 0; ; i++ {
					select {
					case <-stop:
						return
					default:
						current.Store(int64(time.Duration(i%2+1) * time.Minute))
					}
				}
			})
			defer func() { close(stop); writer.Wait() }()
			var readers sync.WaitGroup
			for range 8 {
				readers.Go(func() {
					for range 10 {
						if err := run(t.Context()); err != nil {
							t.Error(err)
						}
					}
				})
			}
			readers.Wait()
		})
	}
}
