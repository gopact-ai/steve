package planner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agentexec"
)

type planningPolicy struct {
	timeout  time.Duration
	attempts int
}
type policyExecutor func(context.Context, agentexec.Spec, string, func(string) error) (agentexec.Result, error)

func (f policyExecutor) Prompt(ctx context.Context, spec agentexec.Spec, prompt string, validate func(string) error) (agentexec.Result, error) {
	return f(ctx, spec, prompt, validate)
}

func checkPlanningPolicy(ctx context.Context, spec agentexec.Spec, want planningPolicy) error {
	var source planningSource
	if err := json.Unmarshal(spec.Source, &source); err != nil {
		return err
	}
	if source.Attempts != want.attempts || spec.Timeout != want.timeout {
		return fmt.Errorf("persisted policy=(%s,%d) want=%+v", spec.Timeout, source.Attempts, want)
	}
	deadline, ok := ctx.Deadline()
	if remaining := time.Until(deadline); !ok || remaining > want.timeout || remaining < want.timeout-time.Second {
		return fmt.Errorf("planning deadline remaining=%s want=%s", remaining, want.timeout)
	}
	return nil
}

func TestLLMPolicyPinsCorrectionRoundsAndRefreshesPlans(t *testing.T) {
	old := &planningPolicy{time.Minute, 2}
	updated := &planningPolicy{2 * time.Minute, 1}
	var current atomic.Pointer[planningPolicy]
	current.Store(old)
	var calls atomic.Int64
	var specs []agentexec.Spec
	l := LLM{Agent: "planner", Timeout: 9 * time.Minute, Attempts: 9, Policy: func() (time.Duration, int) {
		calls.Add(1)
		p := current.Load()
		return p.timeout, p.attempts
	}}
	l.Executor = policyExecutor(func(ctx context.Context, spec agentexec.Spec, _ string, validate func(string) error) (agentexec.Result, error) {
		want := old
		if len(specs) >= 2 {
			want = updated
		}
		if err := checkPlanningPolicy(ctx, spec, *want); err != nil {
			t.Error(err)
		}
		specs = append(specs, spec)
		current.Store(updated)
		return agentexec.Result{Answer: "invalid"}, &agentexec.ValidationError{Cause: validate("invalid")}
	})
	for _, wantCalls := range []int{2, 3} {
		if _, err := l.Plan(t.Context(), Request{TaskID: "task", ProjectID: "project", Goal: "goal"}); err == nil {
			t.Fatal("invalid plan accepted")
		}
		if len(specs) != wantCalls {
			t.Fatalf("policy retry budget not honored: prompts=%d want=%d", len(specs), wantCalls)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("policy sampled %d times for 2 plans", calls.Load())
	}
}

type policyRetainedExecutor struct {
	original agentexec.Spec
	prompt   policyExecutor
	resume   func(context.Context, func(string) error) (agentexec.Result, error)
}

func (e policyRetainedExecutor) OriginalSpec(context.Context, string) (agentexec.Spec, error) {
	return e.original, nil
}
func (e policyRetainedExecutor) Prompt(ctx context.Context, spec agentexec.Spec, text string, validate func(string) error) (agentexec.Result, error) {
	return e.prompt(ctx, spec, text, validate)
}
func (e policyRetainedExecutor) ResumeAttempt(ctx context.Context, _ string, validate func(string) error) (agentexec.Result, error) {
	return e.resume(ctx, validate)
}

func TestLLMPolicyResumeKeepsDurableTimeoutAndRetryBudget(t *testing.T) {
	for _, originalAttempts := range []int{1, 2} {
		t.Run(fmt.Sprint(originalAttempts), func(t *testing.T) {
			initial := &scripted{err: &agentexec.RecoveryBlocked{AttemptID: "original"}}
			originalPolicy := planningPolicy{time.Minute, originalAttempts}
			_, _ = (LLM{Agent: "original-agent", Executor: initial, Policy: func() (time.Duration, int) { return originalPolicy.timeout, originalPolicy.attempts }}).Plan(t.Context(), Request{TaskID: "task", ProjectID: "project", Goal: "goal"})
			if len(initial.specs) != 1 {
				t.Fatal("missing durable spec")
			}
			var prompts int
			executor := policyRetainedExecutor{original: initial.specs[0],
				resume: func(ctx context.Context, validate func(string) error) (agentexec.Result, error) {
					if err := checkPlanningPolicy(ctx, initial.specs[0], originalPolicy); err != nil {
						t.Error(err)
					}
					return agentexec.Result{Answer: "invalid"}, &agentexec.ValidationError{Cause: validate("invalid")}
				},
				prompt: func(ctx context.Context, spec agentexec.Spec, _ string, validate func(string) error) (agentexec.Result, error) {
					prompts++
					if err := checkPlanningPolicy(ctx, spec, originalPolicy); err != nil {
						t.Error(err)
					}
					if spec.Agent != "original-agent" || spec.TurnID != "plan/task/r1/prompt/2" {
						t.Errorf("correction changed durable identity: %+v", spec)
					}
					return agentexec.Result{Answer: "invalid"}, &agentexec.ValidationError{Cause: validate("invalid")}
				},
			}
			l := LLM{Executor: executor, Agent: "new-agent", Timeout: time.Nanosecond, Attempts: 99, Policy: func() (time.Duration, int) {
				t.Error("ResumePlan read live policy")
				return time.Nanosecond, 99
			}}
			if _, err := l.ResumePlan(t.Context(), "original"); err == nil {
				t.Fatal("invalid plan accepted")
			}
			if prompts != originalAttempts-1 {
				t.Fatalf("resume changed retry budget: prompts=%d want=%d", prompts, originalAttempts-1)
			}
		})
	}
}

func TestLLMPolicyDefaultsAndLegacyFields(t *testing.T) {
	for _, tc := range []struct {
		name     string
		policy   func() (time.Duration, int)
		timeout  time.Duration
		attempts int
		want     planningPolicy
	}{
		{"legacy", nil, time.Minute, 1, planningPolicy{time.Minute, 1}},
		{"defaults", nil, 0, 0, planningPolicy{DefaultTimeout, DefaultAttempts}},
		{"source-zero", func() (time.Duration, int) { return 0, 0 }, time.Minute, 1, planningPolicy{DefaultTimeout, DefaultAttempts}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stopped := errors.New("stop after checking")
			l := LLM{Timeout: tc.timeout, Attempts: tc.attempts, Policy: tc.policy, Executor: policyExecutor(func(ctx context.Context, spec agentexec.Spec, _ string, _ func(string) error) (agentexec.Result, error) {
				if err := checkPlanningPolicy(ctx, spec, tc.want); err != nil {
					t.Error(err)
				}
				return agentexec.Result{}, stopped
			})}
			if _, err := l.Plan(t.Context(), Request{TaskID: "task"}); !errors.Is(err, stopped) {
				t.Fatal(err)
			}
		})
	}
}

func TestLLMPolicyConcurrentPlans(t *testing.T) {
	policies := []*planningPolicy{{time.Minute, 1}, {2 * time.Minute, 2}}
	var current atomic.Pointer[planningPolicy]
	current.Store(policies[0])
	l := LLM{Agent: "planner", Policy: func() (time.Duration, int) { p := current.Load(); return p.timeout, p.attempts },
		Executor: policyExecutor(func(ctx context.Context, spec agentexec.Spec, _ string, validate func(string) error) (agentexec.Result, error) {
			var want planningPolicy
			if spec.Timeout == time.Minute {
				want = *policies[0]
			} else {
				want = *policies[1]
			}
			if err := checkPlanningPolicy(ctx, spec, want); err != nil {
				return agentexec.Result{}, err
			}
			return agentexec.Result{Answer: goodPlan}, validate(goodPlan)
		}),
	}
	stop := make(chan struct{})
	var writer sync.WaitGroup
	writer.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				current.Store(policies[i%2])
			}
		}
	})
	defer func() { close(stop); writer.Wait() }()
	var readers sync.WaitGroup
	for range 8 {
		readers.Go(func() {
			for range 20 {
				if _, err := l.Plan(t.Context(), Request{TaskID: "task", Goal: "goal"}); err != nil {
					t.Error(err)
				}
			}
		})
	}
	readers.Wait()
}
