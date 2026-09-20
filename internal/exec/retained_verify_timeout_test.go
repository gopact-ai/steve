package exec

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/agentexec"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/view"
)

// Only the node session is simulated: Verify, Runner, admission, original input,
// recovery, task accounting and artifact workspaces are the production consumers.
type timeoutRetainedVerifier struct {
	tasks         *task.Store
	attempts      *attempt.Service
	entered       chan context.Context
	resumeEntered chan context.Context
	resumeRelease chan struct{}
	prompts       atomic.Int32
	resumes       atomic.Int32
	opens         atomic.Int32
}

func (s *timeoutRetainedVerifier) ID() string { return "ns_verify-timeout" }
func (s *timeoutRetainedVerifier) OpenSession(context.Context, harness.Placement, string, string, []acp.MCPServer) (harness.Runner, error) {
	s.opens.Add(1)
	return s, nil
}
func (*timeoutRetainedVerifier) CloseSession(context.Context, harness.Placement, string) error {
	return nil
}
func (s *timeoutRetainedVerifier) AttachRetainedSession(context.Context, harness.Placement, string, string) (harness.ResumableRunner, error) {
	return s, nil
}
func (*timeoutRetainedVerifier) Cancel(context.Context) error { return nil }
func (*timeoutRetainedVerifier) Abort()                       {}
func (s *timeoutRetainedVerifier) Prompt(ctx context.Context, _ string, _ func(view.Progress)) (string, []string, error) {
	s.prompts.Add(1)
	s.entered <- ctx
	<-ctx.Done()
	return "", nil, errors.Join(harness.ErrStopUnconfirmed, ctx.Err())
}
func (s *timeoutRetainedVerifier) InspectRetained(ctx context.Context) (nodewire.SessionState, error) {
	records, err := s.attempts.Live(ctx)
	if err != nil {
		return nodewire.SessionState{}, err
	}
	if len(records) != 1 {
		return nodewire.SessionState{}, errors.New("expected one retained verifier")
	}
	r := records[0]
	tracked, _ := s.tasks.Get(r.TaskID)
	state := nodewire.SessionState{ID: s.ID(), Harness: r.Harness, State: "running", InputAccepted: 1,
		Binding: nodewire.SessionBinding{ProjectID: r.Project, SessionID: attempt.RetainedSessionID(tracked.Channel, r.TaskID, r.Agent), TaskID: r.TaskID, AttemptID: r.ID, NodeID: r.Node, TaskEpoch: r.Execution.Epoch, ExecutionEpoch: attempt.SessionExecutionEpoch(r)},
		Command: &nodewire.SessionCommand{ID: attempt.InputCommandID(r), InputSequence: 1, State: "running"},
	}
	if r.SessionSettled != nil && *r.SessionSettled {
		state.State, state.Command.State, state.Command.Settled, state.Command.Output = "idle", "completed", true, "PASS"
	}
	return state, nil
}
func (s *timeoutRetainedVerifier) ResumeTurn(ctx context.Context, _ permission.AskFunc, _ acphost.AskUserFunc, _ func(view.Progress)) (string, []string, error) {
	s.resumes.Add(1)
	s.resumeEntered <- ctx
	select {
	case <-s.resumeRelease:
		return "PASS", nil, nil
	case <-ctx.Done():
		return "", nil, errors.Join(harness.ErrStopUnconfirmed, ctx.Err())
	}
}

func TestRetainedVerificationKeepsDurableTimeoutAndAdmission(t *testing.T) {
	p, _, deps, _, tasks := retainedStepWorld(t)
	sessions := &timeoutRetainedVerifier{tasks: tasks, attempts: deps.Attempts, entered: make(chan context.Context, 1), resumeEntered: make(chan context.Context, 1), resumeRelease: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	originalRegistry := execution.New(ctx, tasks)
	runner := agentexec.New(sessions, deps.Roster, deps.Workspaces.(agentexec.Workspaces), deps.Attempts, originalRegistry, retainedStepBudget{tasks})
	verifier := NewVerifiers(nil, runner)
	var policy atomic.Int64
	policy.Store(int64(time.Minute))
	var samples atomic.Int32
	verifier.TimeoutSource = func() time.Duration { samples.Add(1); return time.Duration(policy.Load()) }
	req := StepRequest{TaskID: p.TaskID, PlanID: p.ID, StepID: "work", Project: p.ProjectID, Agent: "builder", Goal: "work"}
	check := plan.Verify{Kind: plan.VerifyAgent, Agent: "shipper"}
	input := plan.StepResult{Answer: "original output"}
	done := make(chan error, 1)
	go func() { done <- verifier.Verify(ctx, req, check, input) }()
	select {
	case promptCtx := <-sessions.entered:
		if err := checkConsumerTimeout(promptCtx, time.Minute, time.Minute, true); err != nil {
			t.Error(err)
		}
	case err := <-done:
		t.Fatalf("new verifier did not start: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("new verifier did not start")
	}
	cancel()
	if err := <-done; !errors.Is(err, harness.ErrStopUnconfirmed) {
		t.Fatalf("initial execution not retained: %v", err)
	}
	records, err := deps.Attempts.ForTask(t.Context(), p.TaskID)
	if err != nil || len(records) != 1 {
		t.Fatalf("records=%+v err=%v", records, err)
	}
	original := records[0]
	saved, err := runner.OriginalSpec(t.Context(), original.ID)
	if err != nil || saved.Timeout != time.Minute {
		t.Fatalf("durable timeout=%s err=%v", saved.Timeout, err)
	}
	shutdown, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	if err := originalRegistry.Shutdown(shutdown); err != nil {
		t.Fatal(err)
	}

	// This is a new observer of the same durable request, not a fake timeout getter.
	registry := execution.New(t.Context(), tasks)
	next := agentexec.New(sessions, deps.Roster, deps.Workspaces.(agentexec.Workspaces), deps.Attempts, registry, retainedStepBudget{tasks})
	verifier = NewVerifiers(nil, next)
	policy.Store(int64(time.Nanosecond))
	verifier.TimeoutSource = func() time.Duration { samples.Add(1); return time.Duration(policy.Load()) }
	resumeCtx, stopResume := context.WithCancel(t.Context())
	defer stopResume()
	go func() { done <- verifier.Verify(resumeCtx, req, check, input) }()
	select {
	case active := <-sessions.resumeEntered:
		if err := checkConsumerTimeout(active, saved.Timeout, saved.Timeout, true); err != nil {
			t.Error(err)
		}
	case err := <-done:
		t.Fatalf("new policy interrupted retained verification before resume: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("retained verification did not resume")
	}
	// A lookup or a resumed observer must not bypass the Runner's active claim.
	var blocked *agentexec.RecoveryBlocked
	if err := verifier.Verify(t.Context(), req, check, input); !errors.As(err, &blocked) || !strings.HasSuffix(blocked.Question.RequestID, "/busy") {
		t.Errorf("concurrent observer bypassed admission fencing: %v", err)
	}
	close(sessions.resumeRelease)
	if err := <-done; err != nil {
		t.Fatalf("retained verification failed: %v", err)
	}
	if err := verifier.Verify(t.Context(), req, check, input); err != nil {
		t.Fatalf("settled result did not replay under durable policy: %v", err)
	}
	after, err := next.OriginalSpec(t.Context(), original.ID)
	if err != nil || !reflect.DeepEqual(after, saved) {
		t.Fatalf("original spec was rewritten: %+v %v", after, err)
	}
	if err := verifier.Verify(t.Context(), req, check, plan.StepResult{Answer: "changed output"}); !errors.As(err, &blocked) || !strings.HasSuffix(blocked.Question.RequestID, "/work") {
		t.Errorf("changed input bypassed work identity fencing: %v", err)
	}
	records, err = deps.Attempts.ForTask(t.Context(), p.TaskID)
	if err != nil || len(records) != 1 || records[0].ID != original.ID || records[0].State != attempt.Bound {
		t.Fatalf("original attempt not preserved: %+v %v", records, err)
	}
	tracked, _ := tasks.Get(p.TaskID)
	if sessions.opens.Load() != 1 || sessions.prompts.Load() != 1 || sessions.resumes.Load() != 1 || tracked.Budget.Turns != 1 {
		t.Fatalf("replayed input or budget: opens=%d prompts=%d resumes=%d turns=%d", sessions.opens.Load(), sessions.prompts.Load(), sessions.resumes.Load(), tracked.Budget.Turns)
	}
	if samples.Load() != 5 {
		t.Fatalf("sampled policy %d times for 5 Verify calls", samples.Load())
	}

	// A genuinely new verification on the same live verifier must still use
	// the freshly sampled policy for both its durable spec and native context.
	policy.Store(int64(2 * time.Minute))
	req.StepID = "new-work"
	newCtx, stopNew := context.WithCancel(t.Context())
	defer stopNew()
	go func() { done <- verifier.Verify(newCtx, req, check, input) }()
	select {
	case promptCtx := <-sessions.entered:
		record, found, err := deps.Attempts.LatestForTurn(t.Context(), req.PlanID+"/"+req.StepID+"/verify")
		if err != nil || !found || record.ID == original.ID {
			t.Errorf("new verification did not receive a new attempt: %+v %v", record, err)
		} else {
			spec, err := next.OriginalSpec(t.Context(), record.ID)
			if err != nil || spec.Timeout != 2*time.Minute {
				t.Errorf("new durable timeout=%s err=%v", spec.Timeout, err)
			}
			if err := checkConsumerTimeout(promptCtx, spec.Timeout, 2*time.Minute, true); err != nil {
				t.Error(err)
			}
		}
	case err := <-done:
		t.Fatalf("new verification did not start: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("new verification did not start")
	}
	stopNew()
	if err := <-done; !errors.Is(err, harness.ErrStopUnconfirmed) {
		t.Fatalf("new observer stop was not retained: %v", err)
	}
	if samples.Load() != 6 || sessions.prompts.Load() != 2 || sessions.resumes.Load() != 1 {
		t.Fatalf("new request sampling/input count: samples=%d prompts=%d resumes=%d", samples.Load(), sessions.prompts.Load(), sessions.resumes.Load())
	}
}
