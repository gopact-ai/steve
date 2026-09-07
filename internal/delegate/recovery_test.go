package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/view"
)

type retainedDelegateSessions struct {
	mu                       sync.Mutex
	attempts                 *attempt.Service
	tasks                    *task.Store
	entered                  chan struct{}
	prompts, resumes, closes int
	inspectErr               error
	answer                   string
	initialResult            bool
	resumeError              error
	stopSettled              bool
	resumeEntered            chan struct{}
}

func (s *retainedDelegateSessions) OpenSession(context.Context, harness.Placement, string, string, []acp.MCPServer) (harness.Runner, error) {
	return s, nil
}
func (s *retainedDelegateSessions) CloseSession(context.Context, harness.Placement, string) error {
	s.mu.Lock()
	s.closes++
	s.mu.Unlock()
	return nil
}
func (s *retainedDelegateSessions) AttachRetainedSession(context.Context, harness.Placement, string, string) (harness.ResumableRunner, error) {
	return s, nil
}
func (s *retainedDelegateSessions) ID() string                   { return "ns_child-original" }
func (s *retainedDelegateSessions) Cancel(context.Context) error { return nil }
func (s *retainedDelegateSessions) Abort()                       {}
func (s *retainedDelegateSessions) Prompt(ctx context.Context, _ string, _ func(view.Progress)) (string, []string, error) {
	s.mu.Lock()
	s.prompts++
	s.mu.Unlock()
	close(s.entered)
	if s.initialResult {
		return s.answer, nil, nil
	}
	<-ctx.Done()
	if s.stopSettled {
		return s.answer, nil, harness.ErrTurnCanceled
	}
	return "", nil, errors.Join(harness.ErrStopUnconfirmed, ctx.Err())
}
func (s *retainedDelegateSessions) InspectRetained(ctx context.Context) (nodewire.SessionState, error) {
	s.mu.Lock()
	err := s.inspectErr
	s.mu.Unlock()
	if err != nil {
		return nodewire.SessionState{}, err
	}
	records, err := s.attempts.Live(ctx)
	if err != nil || len(records) != 1 {
		return nodewire.SessionState{}, errors.New("expected one original attempt")
	}
	r := records[0]
	tracked, _ := s.tasks.Get(r.TaskID)
	var epoch uint64
	for _, lease := range r.Leases {
		if lease.Key == "attempt:"+r.ID {
			epoch = lease.Epoch
		}
	}
	return nodewire.SessionState{ID: s.ID(), Harness: r.Harness, State: "running", InputAccepted: 1, Binding: nodewire.SessionBinding{ProjectID: r.Project, SessionID: attempt.RetainedSessionID(tracked.Channel, tracked.ID, r.Agent), TaskID: r.TaskID, AttemptID: r.ID, NodeID: r.Node, ExecutionEpoch: epoch, TaskEpoch: r.Execution.Epoch}, Command: &nodewire.SessionCommand{ID: r.TurnID, InputSequence: 1, State: "running"}}, nil
}
func (s *retainedDelegateSessions) ResumeTurn(ctx context.Context, _ permission.AskFunc, _ acphost.AskUserFunc, progress func(view.Progress)) (string, []string, error) {
	s.mu.Lock()
	s.resumes++
	s.mu.Unlock()
	if progress != nil {
		progress(view.Progress{Usage: view.Usage{Reported: true, InputTokens: 31, OutputTokens: 9}, Settings: view.Settings{Model: "mock-model"}})
	}
	if s.resumeEntered != nil {
		close(s.resumeEntered)
		<-ctx.Done()
		if s.stopSettled {
			return s.answer, nil, harness.ErrTurnCanceled
		}
		return "", nil, errors.Join(harness.ErrStopUnconfirmed, ctx.Err())
	}
	return s.answer, nil, s.resumeError
}

func detachedDelegateFixture(t *testing.T) (*world, *retainedDelegateSessions, task.Task, agentmcp.DelegateResult) {
	t.Helper()
	w, _ := executionWorld(t)
	lifetime, stop := context.WithCancel(t.Context())
	registry := execution.New(lifetime, w.tasks)
	t.Cleanup(func() {
		stop()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = registry.Shutdown(ctx)
	})
	w.service.SetExecution(registry)
	w.service.artifacts.SetExecution(registry)
	sessions := &retainedDelegateSessions{attempts: w.attempts, tasks: w.tasks, entered: make(chan struct{}), answer: strings.Repeat("full original output ", 300)}
	w.service.sessions = sessions
	w.service.SetGate(nil)
	parent := w.running(t, "codex")
	child, err := w.service.Start(t.Context(), "chat", "codex", agentmcp.DelegateRequest{Goal: "work", Agent: "builder"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-sessions.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("child did not start")
	}
	records, err := w.attempts.ForTask(t.Context(), child.TaskID)
	if err != nil || len(records) != 1 || records[0].Session != sessions.ID() {
		t.Fatalf("original node session was not durably recorded: %+v %v", records, err)
	}
	stop()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := registry.Shutdown(ctx); err != nil {
		t.Fatalf("detached service observer did not join: %v", err)
	}
	return w, sessions, parent, child
}
func TestManagedDelegateDetachKeepsOriginalTaskBudgetAndAttemptOpen(t *testing.T) {
	w, sessions, _, child := detachedDelegateFixture(t)
	stored, _ := w.tasks.Get(child.TaskID)
	if stored.State != task.StateRunning || stored.Result != nil || len(stored.Attempts) != 1 || !stored.Attempts[0].Open() || stored.Budget.Tokens.Total != 0 {
		t.Fatalf("observer detach terminalized or charged the child: %+v", stored)
	}
	records, _ := w.attempts.ForTask(t.Context(), child.TaskID)
	if records[0].State != attempt.Running {
		t.Fatalf("original attempt lost: %+v", records[0])
	}
	sessions.mu.Lock()
	defer sessions.mu.Unlock()
	if sessions.closes != 0 {
		t.Fatal("observer detach closed the node-owned execution")
	}
}

func recoveredDelegateService(t *testing.T, w *world, sessions Sessions) *Service {
	t.Helper()
	s := New(w.tasks, w.service.roster, sessions, w.service.assembler, w.service.workspaces, "hub-b")
	s.SetLedger(w.attempts, w.service.artifacts)
	registry := execution.New(t.Context(), w.tasks)
	s.SetExecution(registry)
	s.artifacts.SetExecution(registry)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = registry.Shutdown(ctx)
	})
	return s
}
func awaitDelegateResult(t *testing.T, tasks *task.Store, id string) task.Task {
	t.Helper()
	until := time.Now().Add(5 * time.Second)
	for time.Now().Before(until) {
		stored, _ := tasks.Get(id)
		if stored.Result != nil {
			return stored
		}
		time.Sleep(5 * time.Millisecond)
	}
	stored, _ := tasks.Get(id)
	t.Fatalf("child never settled: %+v", stored)
	return task.Task{}
}
func TestRetainedDelegateResumesOriginalAttemptAndDeliversFullResultOnce(t *testing.T) {
	w, sessions, parent, child := detachedDelegateFixture(t)
	service := recoveredDelegateService(t, w, sessions)
	var mu sync.Mutex
	var deliveries []Delivery
	service.SetDeliverer(func(_ context.Context, d Delivery) error {
		mu.Lock()
		deliveries = append(deliveries, d)
		mu.Unlock()
		return nil
	})
	var scans sync.WaitGroup
	for range 8 {
		scans.Go(func() {
			if err := service.RecoverRetained(t.Context()); err != nil {
				t.Error(err)
			}
		})
	}
	scans.Wait()
	stored := awaitDelegateResult(t, w.tasks, child.TaskID)
	if stored.State != task.StateDone || stored.Result.Answer != sessions.answer || len(stored.Attempts) != 1 || stored.Attempts[0].Open() {
		t.Fatalf("resumed original child: %+v", stored)
	}
	records, err := w.attempts.ForTask(t.Context(), child.TaskID)
	if err != nil || len(records) != 1 || records[0].State != attempt.Bound || records[0].Session != sessions.ID() || len(records[0].Result.Output) == 0 {
		t.Fatalf("original attempt not completed: %+v %v", records, err)
	}
	if stored.Budget.Tokens.Total != 40 {
		t.Fatalf("child budget charged %d, want 40", stored.Budget.Tokens.Total)
	}
	charged, _ := w.tasks.Get(parent.ID)
	if charged.Budget.Tokens.Total != 40 {
		t.Fatalf("parent charged %d, want 40", charged.Budget.Tokens.Total)
	}
	for range 3 {
		if err := service.RecoverRetained(t.Context()); err != nil {
			t.Fatal(err)
		}
		service.RedeliverPending(t.Context())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(deliveries) != 1 || len(deliveries[0].Children) != 1 || deliveries[0].Children[0].Answer != sessions.answer {
		t.Fatalf("delivery mismatch: %+v", deliveries)
	}
	sessions.mu.Lock()
	defer sessions.mu.Unlock()
	if sessions.prompts != 1 || sessions.resumes != 1 {
		t.Fatalf("prompt replay or concurrent observers: prompt=%d resume=%d", sessions.prompts, sessions.resumes)
	}
}
func TestRetainedDelegateUncertainNodeAsksOnceAndNeverSettlesOrReplays(t *testing.T) {
	w, sessions, _, child := detachedDelegateFixture(t)
	sessions.inspectErr = errors.New("node unreachable")
	service := recoveredDelegateService(t, w, sessions)
	notices := make(chan RecoveryQuestion, 4)
	service.SetRecoveryQuestion(func(ctx context.Context, q RecoveryQuestion) (view.Answer, error) {
		notices <- q
		return view.Answer{Value: "wait"}, nil
	})
	if err := service.RecoverRetained(t.Context()); err != nil {
		t.Fatal(err)
	}
	var question RecoveryQuestion
	select {
	case question = <-notices:
	case <-time.After(3 * time.Second):
		t.Fatal("missing recovery question")
	}
	if question.Task != child.TaskID || question.Question.Kind != "recovery" || !strings.Contains(question.Question.Message, "原节点") {
		t.Fatalf("bad recovery question: %+v", question)
	}
	for range 4 {
		time.Sleep(10 * time.Millisecond)
		if err := service.RecoverRetained(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case q := <-notices:
		t.Fatalf("duplicate unchanged recovery question: %+v", q)
	case <-time.After(40 * time.Millisecond):
	}
	stored, _ := w.tasks.Get(child.TaskID)
	if stored.Result != nil || stored.State != task.StateRunning || !stored.Attempts[0].Open() || stored.Budget.Tokens.Total != 0 {
		t.Fatalf("uncertainty was terminalized: %+v", stored)
	}
	sessions.mu.Lock()
	sessions.inspectErr = nil
	sessions.mu.Unlock()
	if err := service.RecoverRetained(t.Context()); err != nil {
		t.Fatal(err)
	}
	stored = awaitDelegateResult(t, w.tasks, child.TaskID)
	if stored.Result.Answer != sessions.answer {
		t.Fatal("same child failed to recover after node returned")
	}
}

func TestRetainedDelegateRestoresBoundResultBeforeTaskPersistenceWithoutChargingTwice(t *testing.T) {
	w, _ := executionWorld(t)
	parent := w.running(t, "codex")
	tracked, err := w.tasks.Spawn(parent.ID, task.Task{Goal: "already completed", Member: "builder", Node: "node-a", Origin: "delegate:" + parent.ID, ProjectID: "p", Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.tasks.Begin(tracked.ID, "builder", "node-a", "ns_completed-child"); err != nil {
		t.Fatal(err)
	}
	token, err := w.tasks.ExecutionToken(tracked.ID)
	if err != nil {
		t.Fatal(err)
	}
	record, err := w.attempts.Open(t.Context(), attempt.Spec{ID: "completed-delegate", Execution: &token, TaskID: tracked.ID, TurnID: "delegate/" + tracked.ID, Kind: attempt.KindDelegate, Project: "p", Node: "node-a", Agent: "builder", Harness: "mock", Scope: attempt.ScopePathSet, Workspace: project.Workspace{ID: "completed-workspace", Project: "p", Node: "node-a", Path: tracked.Workspace, Kind: project.KindWorktree}})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.tasks.BindAttempt(token, record.ID, record.TurnID); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []attempt.State{attempt.Prepared, attempt.Running} {
		record, err = w.attempts.Advance(t.Context(), record.ID, phase, "test", func(record *attempt.Record) { record.Session = "ns_completed-child" })
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := w.attempts.MarkSessionSettled(t.Context(), record.ID, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err = w.attempts.Advance(t.Context(), record.ID, attempt.BindReady, "test", nil); err != nil {
		t.Fatal(err)
	}
	complete := agentmcp.DelegateResult{TaskID: tracked.ID, Agent: "builder", Node: "node-a", Outcome: "ok", Answer: strings.Repeat("complete reply ", 600)}
	raw, err := json.Marshal(complete)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.attempts.Complete(t.Context(), record.ID, "test", attempt.Completion{Result: attempt.Result{Summary: "complete reply", Output: raw}, Usage: &attempt.Usage{Reported: true, Input: 31, Output: 9, Model: "mock-model"}}); err != nil {
		t.Fatal(err)
	}
	before, _ := w.tasks.Get(tracked.ID)
	if before.Result != nil || before.Budget.Tokens.Total != 0 {
		t.Fatal("fixture already persisted task outcome")
	}
	service := recoveredDelegateService(t, w, w.sessions)
	var mu sync.Mutex
	delivered := 0
	service.SetDeliverer(func(_ context.Context, d Delivery) error {
		mu.Lock()
		delivered++
		mu.Unlock()
		if d.Children[0].Answer != complete.Answer {
			t.Error("full committed output was truncated")
		}
		return nil
	})
	if err := service.RecoverRetained(t.Context()); err != nil {
		t.Fatal(err)
	}
	after := awaitDelegateResult(t, w.tasks, tracked.ID)
	if after.Result.Answer != complete.Answer || after.Budget.Tokens.Total != 40 || after.Attempts[0].Open() {
		t.Fatalf("result or charge not restored: %+v", after)
	}
	service.RedeliverPending(t.Context())
	if err := service.RecoverRetained(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, _ = w.tasks.Get(tracked.ID)
	charged, _ := w.tasks.Get(parent.ID)
	if after.Budget.Tokens.Total != 40 || charged.Budget.Tokens.Total != 40 {
		t.Fatal("completion was charged twice")
	}
	mu.Lock()
	defer mu.Unlock()
	if delivered != 1 {
		t.Fatalf("delivered %d times", delivered)
	}
	w.sessions.mu.Lock()
	defer w.sessions.mu.Unlock()
	if len(w.sessions.opened) != 0 || len(w.sessions.prompts) != 0 {
		t.Fatal("committed result reopened an agent")
	}
}

func TestRetainedDelegatePublicationFailureKeepsOriginalResultAndBudgetUnresolved(t *testing.T) {
	w, sessions, _, child := detachedDelegateFixture(t)
	records, err := w.attempts.ForTask(t.Context(), child.TaskID)
	if err != nil || len(records) != 1 {
		t.Fatal(err)
	}
	workspace := records[0].Workspace.Path
	if err := os.RemoveAll(workspace); err != nil {
		t.Fatal(err)
	}
	service := recoveredDelegateService(t, w, sessions)
	notices := make(chan RecoveryQuestion, 2)
	service.SetRecoveryQuestion(func(_ context.Context, q RecoveryQuestion) (view.Answer, error) {
		notices <- q
		return view.Answer{Value: "wait"}, nil
	})
	if err := service.RecoverRetained(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case q := <-notices:
		if !strings.Contains(q.Question.Message, "产物") {
			t.Fatalf("missing publication context: %+v", q)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("publication failure was not surfaced")
	}
	stored, _ := w.tasks.Get(child.TaskID)
	if stored.State != task.StateRunning || stored.Result != nil || !stored.Attempts[0].Open() || stored.Budget.Tokens.Total != 0 {
		t.Fatalf("uncommitted result was terminalized: %+v", stored)
	}
	current, err := w.attempts.Get(t.Context(), records[0].ID)
	if err != nil || current.State != attempt.Running {
		t.Fatalf("retained command was discarded: %+v %v", current, err)
	}
	if err := os.MkdirAll(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	if err := service.RecoverRetained(t.Context()); err != nil {
		t.Fatal(err)
	}
	stored = awaitDelegateResult(t, w.tasks, child.TaskID)
	if stored.Result.Answer != sessions.answer || stored.Budget.Tokens.Total != 40 {
		t.Fatal("retained result did not settle after restoring the workspace")
	}
	sessions.mu.Lock()
	defer sessions.mu.Unlock()
	if sessions.prompts != 1 {
		t.Fatal("publication retry replayed prompt")
	}
}

func TestRetainedDelegateRetriesFailedQuestionDeliveryAndKeepsVisibleDiagnosis(t *testing.T) {
	w, sessions, _, child := detachedDelegateFixture(t)
	sessions.inspectErr = errors.New("node unavailable")
	service := recoveredDelegateService(t, w, sessions)
	calls := make(chan RecoveryQuestion, 3)
	var mu sync.Mutex
	count := 0
	service.SetRecoveryQuestion(func(_ context.Context, q RecoveryQuestion) (view.Answer, error) {
		mu.Lock()
		count++
		number := count
		mu.Unlock()
		calls <- q
		if number == 1 {
			return view.Answer{}, errors.New("question channel unavailable")
		}
		return view.Answer{Value: "wait"}, nil
	})
	if err := service.RecoverRetained(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-calls:
	case <-time.After(3 * time.Second):
		t.Fatal("missing first question delivery")
	}
	until := time.Now().Add(3 * time.Second)
	retried := false
	for time.Now().Before(until) {
		if err := service.RecoverRetained(t.Context()); err != nil {
			t.Fatal(err)
		}
		select {
		case <-calls:
			retried = true
		case <-time.After(10 * time.Millisecond):
		}
		if retried {
			break
		}
	}
	if !retried {
		t.Fatal("failed question delivery permanently suppressed recovery question")
	}
	records, err := w.attempts.ForTask(t.Context(), child.TaskID)
	if err != nil || len(records) != 1 || !records[0].Unsettled || !strings.Contains(records[0].Error, "原节点") {
		t.Fatalf("missing durable recovery diagnosis: %+v %v", records, err)
	}
}

func TestRetainedDelegateExplicitStopDoesNotRestartOrAskForRecovery(t *testing.T) {
	w, sessions, _, child := detachedDelegateFixture(t)
	if _, err := w.tasks.SetAside(child.TaskID, task.StateCancelled); err != nil {
		t.Fatal(err)
	}
	service := recoveredDelegateService(t, w, sessions)
	service.SetRecoveryQuestion(func(context.Context, RecoveryQuestion) (view.Answer, error) {
		t.Error("cancelled child requested recovery")
		return view.Answer{}, nil
	})
	if err := service.RecoverRetained(t.Context()); err != nil {
		t.Fatal(err)
	}
	result, err := service.Await(t.Context(), "chat", "codex", agentmcp.AwaitRequest{TaskID: child.TaskID, WaitSeconds: 1})
	if err != nil || result.State != "failed" {
		t.Fatalf("explicit stop was hidden: %+v %v", result, err)
	}
	sessions.mu.Lock()
	defer sessions.mu.Unlock()
	if sessions.resumes != 0 {
		t.Fatal("explicitly stopped execution was resumed")
	}
}

type settledDelegateError struct{}

func (settledDelegateError) Error() string       { return "native command failed after producing output" }
func (settledDelegateError) PromptSettled() bool { return true }
func TestRetainedDelegateFailedNativeCommandPreservesCompleteOutput(t *testing.T) {
	w, sessions, _, child := detachedDelegateFixture(t)
	sessions.resumeError = settledDelegateError{}
	service := recoveredDelegateService(t, w, sessions)
	if err := service.RecoverRetained(t.Context()); err != nil {
		t.Fatal(err)
	}
	stored := awaitDelegateResult(t, w.tasks, child.TaskID)
	if stored.State != task.StateFailed || stored.Result.Answer != sessions.answer || stored.Budget.Tokens.Total != 40 {
		t.Fatalf("native failure lost its output or budget: %+v", stored)
	}
	records, err := w.attempts.ForTask(t.Context(), child.TaskID)
	if err != nil || len(records) != 1 || records[0].State != attempt.Failed || records[0].Result == nil {
		t.Fatalf("failed native result not committed: %+v %v", records, err)
	}
	var result agentmcp.DelegateResult
	if err := json.Unmarshal(records[0].Result.Output, &result); err != nil || result.Answer != sessions.answer {
		t.Fatalf("complete native failure output missing: %+v %v", result, err)
	}
}

func TestManagedDelegateExplicitStopReceiptSettlesCancelledExecution(t *testing.T) {
	for _, state := range []task.State{task.StatePaused, task.StateCancelled} {
		t.Run(string(state), func(t *testing.T) {
			w, registry := executionWorld(t)
			sessions := &retainedDelegateSessions{attempts: w.attempts, tasks: w.tasks, entered: make(chan struct{}), answer: "partial before explicit stop", stopSettled: true}
			w.service.sessions = sessions
			w.service.SetGate(nil)
			w.running(t, "codex")
			child, err := w.service.Start(t.Context(), "chat", "codex", agentmcp.DelegateRequest{Goal: "work", Agent: "builder"})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-sessions.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("native input never started")
			}
			ids, err := w.tasks.SetAside(child.TaskID, state)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			if err := registry.Stop(ids, task.ErrExecutionStopped).Wait(ctx); err != nil {
				t.Fatalf("settled native stop stayed unresolved: %v", err)
			}
			stored, _ := w.tasks.Get(child.TaskID)
			if stored.State != state || stored.Attempts[0].Open() || stored.Result == nil || stored.Result.Outcome != "cancelled" || stored.Result.Answer != sessions.answer {
				t.Fatalf("explicit stop was not settled: %+v", stored)
			}
			records, err := w.attempts.ForTask(t.Context(), child.TaskID)
			if err != nil || records[0].State != attempt.Failed || records[0].Unsettled || records[0].SessionSettled == nil || !*records[0].SessionSettled {
				t.Fatalf("missing native settlement: %+v %v", records, err)
			}
		})
	}
}
func TestRecoveredDelegateExplicitStopReceiptUsesIndependentSettlementContext(t *testing.T) {
	w, sessions, _, child := detachedDelegateFixture(t)
	sessions.stopSettled = true
	sessions.resumeEntered = make(chan struct{})
	service := recoveredDelegateService(t, w, sessions)
	if err := service.RecoverRetained(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sessions.resumeEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("original command was not resumed")
	}
	ids, err := w.tasks.SetAside(child.TaskID, task.StatePaused)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := service.executions.Stop(ids, task.ErrExecutionStopped).Wait(ctx); err != nil {
		t.Fatalf("settled recovered native stop stayed unresolved: %v", err)
	}
	stored, _ := w.tasks.Get(child.TaskID)
	if stored.State != task.StatePaused || stored.Attempts[0].Open() || stored.Result == nil || stored.Result.Outcome != "cancelled" || stored.Budget.Tokens.Total != 40 {
		t.Fatalf("recovered stop lost settlement: %+v", stored)
	}
}
