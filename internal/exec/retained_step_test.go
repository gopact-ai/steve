package exec

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/agentexec"
	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/view"
)

type retainedStepSessions struct {
	mu                        sync.Mutex
	attempts                  *attempt.Service
	tasks                     *task.Store
	entered                   chan struct{}
	prompted, resumed, closed int
	answer                    string
	inspectErr                error
	book                      *ledger.Ledger
}

func (s *retainedStepSessions) ID() string { return "ns_step-original" }
func (s *retainedStepSessions) OpenSession(context.Context, harness.Placement, string, string, []acp.MCPServer) (harness.Runner, error) {
	return s, nil
}
func (s *retainedStepSessions) CloseSession(context.Context, harness.Placement, string) error {
	s.mu.Lock()
	s.closed++
	s.mu.Unlock()
	return nil
}
func (s *retainedStepSessions) AttachRetainedSession(context.Context, harness.Placement, string, string) (harness.ResumableRunner, error) {
	return s, nil
}
func (s *retainedStepSessions) Cancel(context.Context) error { return nil }
func (s *retainedStepSessions) Abort()                       {}
func (s *retainedStepSessions) Prompt(ctx context.Context, _ string, _ func(view.Progress)) (string, []string, error) {
	s.mu.Lock()
	s.prompted++
	s.mu.Unlock()
	close(s.entered)
	<-ctx.Done()
	return "", nil, errors.Join(harness.ErrStopUnconfirmed, ctx.Err())
}
func (s *retainedStepSessions) InspectRetained(ctx context.Context) (nodewire.SessionState, error) {
	if s.inspectErr != nil {
		return nodewire.SessionState{}, s.inspectErr
	}
	records, err := s.attempts.Live(ctx)
	if err != nil || len(records) != 1 {
		return nodewire.SessionState{}, errors.New("one original step required")
	}
	r := records[0]
	tracked, _ := s.tasks.Get(r.TaskID)
	state := nodewire.SessionState{ID: s.ID(), Harness: r.Harness, State: "running", InputAccepted: 1, Binding: nodewire.SessionBinding{ProjectID: r.Project, SessionID: attempt.RetainedSessionID(tracked.Channel, tracked.ID, r.Agent), TaskID: r.TaskID, AttemptID: r.ID, NodeID: r.Node, ExecutionEpoch: attempt.SessionExecutionEpoch(r), TaskEpoch: r.Execution.Epoch}, Command: &nodewire.SessionCommand{ID: attempt.InputCommandID(r), InputSequence: 1, State: "running"}}
	if r.SessionSettled != nil && *r.SessionSettled {
		state.State = "closed"
		state.ProcessStopped = true
		state.Command.State = "completed"
		state.Command.Settled = true
		state.Command.ProcessStopped = true
		state.Command.Output = s.answer
	}
	return state, nil
}
func (s *retainedStepSessions) ResumeTurn(_ context.Context, _ permission.AskFunc, _ acphost.AskUserFunc, p func(view.Progress)) (string, []string, error) {
	s.mu.Lock()
	s.resumed++
	s.mu.Unlock()
	p(view.Progress{Usage: view.Usage{Reported: true, InputTokens: 23, OutputTokens: 7}})
	return s.answer, nil, nil
}

type retainedStepBudget struct{ tasks *task.Store }

func (b retainedStepBudget) Reserve(id string) (int, time.Time, error) {
	_, err := b.tasks.ReserveTurn(id)
	return 0, time.Time{}, err
}
func (b retainedStepBudget) ReserveAttempt(record attempt.Record) (int, time.Time, error) {
	_, err := b.tasks.ReserveAttempt(*record.Execution, record.ID, record.TurnID, record.Agent, record.Node, record.StartedAt)
	return 0, time.Time{}, err
}
func (b retainedStepBudget) SettleAttempt(record attempt.Record, outcome task.Outcome) error {
	var usage task.RecoveryUsage
	if u := record.Usage; u != nil {
		usage = task.RecoveryUsage{Tokens: task.Tokens{Input: u.Input, Output: u.Output, CachedRead: u.CachedRead, CachedWrite: u.CachedWrite}, Model: u.Model, Reported: u.Reported}
	}
	return b.tasks.SettleAttempt(record.TaskID, record.ID, record.TurnID, record.EndedAt, outcome, usage)
}

func retainedStepFixture(t *testing.T, checks ...*plan.Verify) (plan.Plan, plan.Step, Deps, *retainedStepSessions, *task.Store) {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	tasks, err := task.OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	owner, err := tasks.Create(task.Task{Goal: "original plan", Channel: "console:plan", ProjectID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	projects := project.Open(book)
	if err := projects.Declare(t.Context(), []project.Project{{ID: "p", Home: project.Home{Path: t.TempDir()}}}); err != nil {
		t.Fatal(err)
	}
	artifacts := artifact.New(t.TempDir(), book, projects, artifact.LocalNodes{Dir: t.TempDir()})
	attempts := attempt.New(book)
	fleet := testRoster(t, bothNodes())
	sessions := &retainedStepSessions{book: book, attempts: attempts, tasks: tasks, entered: make(chan struct{}), answer: "  original complete step\nREF: git abcdef\n"}
	runner := NewAgentRunner(sessions, nil, fleet)
	lifetime, stop := context.WithCancel(t.Context())
	registry := execution.New(lifetime, tasks)
	t.Cleanup(func() {
		stop()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = registry.Shutdown(ctx)
	})
	deps := Deps{Executions: registry, Roster: fleet, Runner: runner, Budget: retainedStepBudget{tasks}, Workspaces: artifacts, Attempts: attempts, Artifacts: artifacts}
	p := plan.Plan{ID: "retained-plan", Rev: 1, TaskID: owner.ID, ProjectID: "p", Goal: owner.Goal}
	work := step("step-1", "work", []string{"gpu"})
	work.Agent = "builder"
	if len(checks) > 0 {
		work.Verify = checks[0]
	}
	done := make(chan error, 1)
	go func() { _, err := runStepWithRecovery(lifetime, p, work, nil, deps); done <- err }()
	select {
	case <-sessions.entered:
	case err := <-done:
		t.Fatalf("step ended before its native input: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("step did not start")
	}
	records, err := attempts.Live(t.Context())
	if err != nil || len(records) != 1 || records[0].Session != sessions.ID() {
		stop()
		<-done
		t.Fatalf("step native identity missing: %+v %v", records, err)
	}
	stop()
	select {
	case err := <-done:
		if !errors.Is(err, harness.ErrStopUnconfirmed) {
			t.Fatalf("step was not retained: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("step observer remained")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := registry.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	deps.Executions = execution.New(t.Context(), tasks)
	return p, work, deps, sessions, tasks
}
func TestStepRetainedRecoveryKeepsSameAttemptAndDoesNotReserveAgain(t *testing.T) {
	p, work, deps, sessions, tasks := retainedStepFixture(t)
	result, err := runStepWithRecovery(t.Context(), p, work, nil, deps)
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != sessions.answer || !result.Verified || result.AttemptID == "" {
		t.Fatalf("missing original result: %+v", result)
	}
	records, err := deps.Attempts.ForTask(t.Context(), p.TaskID)
	if err != nil || len(records) != 1 || records[0].State != attempt.Bound {
		t.Fatalf("old step not bound: %+v %v", records, err)
	}
	tracked, _ := tasks.Get(p.TaskID)
	if tracked.Budget.Turns != 1 || tracked.Budget.Tokens.Total != 30 || len(tracked.Attempts) != 1 || tracked.Attempts[0].Open() {
		t.Fatalf("step budget reserved %d times", tracked.Budget.Turns)
	}
	again, err := runStepWithRecovery(t.Context(), p, work, nil, deps)
	if err != nil || again.AttemptID != result.AttemptID {
		t.Fatalf("projection replay=%+v %v", again, err)
	}
	sessions.mu.Lock()
	defer sessions.mu.Unlock()
	if sessions.prompted != 1 || sessions.resumed != 1 {
		t.Fatalf("native step replayed: %d/%d", sessions.prompted, sessions.resumed)
	}
}

func TestRetainedStepStorageFailureResumesWithoutNativeReplay(t *testing.T) {
	for _, phase := range []string{"snapshotted", "published", "durable", "verifying", "bind-ready", "bound", "accounting"} {
		t.Run(phase, func(t *testing.T) {
			p, work, deps, sessions, tasks := retainedStepFixture(t, &plan.Verify{Kind: plan.VerifyAgent, Agent: "shipper"})
			verifyCalls := 0
			deps.Verifier = verifyFunc(func(StepRequest) error { verifyCalls++; return nil })
			cut := fmt.Sprintf(`CREATE TRIGGER cut BEFORE UPDATE OF state ON operations WHEN NEW.kind='attempt' AND NEW.state='%s' AND OLD.state!=NEW.state BEGIN SELECT RAISE(FAIL,'phase write unavailable'); END`, phase)
			if phase == "accounting" {
				cut = fmt.Sprintf(`CREATE TRIGGER cut BEFORE UPDATE OF data ON bindings WHEN NEW.kind='document' AND NEW.id='tasks' AND json_extract(NEW.data,'$.tasks."%s".budget.tokens.total')>0 BEGIN SELECT RAISE(FAIL,'accounting unavailable'); END`, p.TaskID)
			}
			if err := sessions.book.Update(t.Context(), func(tx *ledger.Tx) error { _, err := tx.Exec(cut); return err }); err != nil {
				t.Fatal(err)
			}
			_, err := runStepWithRecovery(t.Context(), p, work, nil, deps)
			var blocked *agentexec.RecoveryBlocked
			if !errors.As(err, &blocked) {
				t.Fatalf("injected failure did not retain original step: %v", err)
			}
			if err := sessions.book.Update(t.Context(), func(tx *ledger.Tx) error { _, err := tx.Exec("DROP TRIGGER cut"); return err }); err != nil {
				t.Fatal(err)
			}
			result, err := runStepWithRecovery(t.Context(), p, work, nil, deps)
			if err != nil || result.Answer != sessions.answer || !result.Verified {
				t.Fatalf("same step not completed: %+v %v", result, err)
			}
			tracked, _ := tasks.Get(p.TaskID)
			if tracked.Budget.Turns != 1 || tracked.Budget.Tokens.Total != 30 || len(tracked.Attempts) != 1 || tracked.Attempts[0].Open() || sessions.prompted != 1 || verifyCalls != 1 {
				t.Fatalf("recovery repeated input, budget or verifier: task=%+v native=%d verifies=%d", tracked, sessions.prompted, verifyCalls)
			}
		})
	}
}

func TestRetainedStepCannotReplayUnconfirmedShellVerification(t *testing.T) {
	p, work, deps, sessions, _ := retainedStepFixture(t, &plan.Verify{Kind: plan.VerifyCommand, Command: "deploy-and-check"})
	verifyCalls := 0
	deps.Verifier = verifyFunc(func(StepRequest) error {
		verifyCalls++
		return errors.Join(harness.ErrStopUnconfirmed, errors.New("connection disappeared"))
	})
	for range 2 {
		_, err := runStepWithRecovery(t.Context(), p, work, nil, deps)
		var blocked *agentexec.RecoveryBlocked
		if !errors.As(err, &blocked) {
			t.Fatalf("unknown shell settlement was not retained: %v", err)
		}
	}
	if sessions.prompted != 1 || sessions.resumed != 1 || verifyCalls != 1 {
		t.Fatalf("unknown command replayed: prompts=%d observes=%d verifies=%d", sessions.prompted, sessions.resumed, verifyCalls)
	}
}

type uncertainStepOpenSessions struct {
	attempts *attempt.Service
	opens    int
}

func (s *uncertainStepOpenSessions) OpenSession(ctx context.Context, _ harness.Placement, _, _ string, _ []acp.MCPServer) (harness.Runner, error) {
	s.opens++
	key, _ := execution.KeyOf(ctx)
	r, err := s.attempts.Get(ctx, key.AttemptID)
	if err != nil {
		return nil, err
	}
	if r.SessionSettled == nil || *r.SessionSettled {
		return nil, errors.New("step open was not durably armed")
	}
	return nil, &harness.NodeSessionOpenUncertain{Binding: nodewire.SessionBinding{TaskID: r.TaskID, AttemptID: r.ID, ProjectID: r.Project, NodeID: r.Node, TaskEpoch: r.Execution.Epoch, ExecutionEpoch: attempt.SessionExecutionEpoch(r)}, OpenCommandID: attempt.InputCommandID(r) + "/open", Cause: errors.New("open reply lost")}
}
func (*uncertainStepOpenSessions) CloseSession(context.Context, harness.Placement, string) error {
	return nil
}

func TestUncertainStepOpenCanDetachWithoutReplayingOrSettling(t *testing.T) {
	w := recoveryWorld(t)
	sessions := &uncertainStepOpenSessions{attempts: w.sup.deps.Attempts}
	deps := w.sup.deps
	deps.Runner = NewAgentRunner(sessions, nil, deps.Roster)
	deps.Budget = retainedStepBudget{tasks: w.tasks}
	lifetime, stop := context.WithCancel(t.Context())
	deps.Executions = execution.New(lifetime, w.tasks)
	work := w.plan.Steps[0]
	work.Agent = "builder"
	for range 2 {
		_, err := runStepWithRecovery(t.Context(), w.plan, work, nil, deps)
		if err == nil {
			t.Fatal("uncertain open completed or replayed")
		}
	}
	tracked, _ := w.tasks.Get(w.plan.TaskID)
	if sessions.opens != 1 || tracked.Budget.Turns != 1 || len(tracked.Attempts) != 1 || !tracked.Attempts[0].Open() {
		t.Fatalf("uncertain step open replayed or settled: opens=%d task=%+v", sessions.opens, tracked)
	}
	stop()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := deps.Executions.Shutdown(ctx); err != nil {
		t.Fatalf("node step preparation blocked coordinator shutdown: %v", err)
	}
}

type settledFailedStep struct {
	*AgentRunner
	sessions *retainedStepSessions
}

func (r settledFailedStep) ResumeStep(ctx context.Context, _ StepRequest, _ attempt.Record, attached func(nodewire.SessionState) error) (plan.StepResult, error) {
	state, err := r.sessions.InspectRetained(ctx)
	if err != nil {
		return plan.StepResult{}, err
	}
	state.Command.Settled, state.Command.State, state.State = true, "cancelled", "idle"
	if err := attached(state); err != nil {
		return plan.StepResult{}, err
	}
	return plan.StepResult{}, harness.ErrTurnCanceled
}

func TestRetainedSettledFailureReleasesOriginalNodeSession(t *testing.T) {
	p, work, deps, sessions, tasks := retainedStepFixture(t)
	deps.Runner = settledFailedStep{AgentRunner: deps.Runner.(*AgentRunner), sessions: sessions}
	_, _ = runStepWithRecovery(t.Context(), p, work, nil, deps)
	records, err := deps.Attempts.ForTask(t.Context(), p.TaskID)
	if err != nil || len(records) != 1 || records[0].State != attempt.Failed || sessions.closed != 1 {
		t.Fatalf("settled failed execution leaked original native session: records=%+v err=%v closes=%d", records, err, sessions.closed)
	}
	tracked, _ := tasks.Get(p.TaskID)
	if tracked.Attempts[0].Open() {
		t.Fatal("settled failed execution retained open accounting")
	}
}
func TestStepRetainedUnreachableIsRecoveryQuestionNotNewAttempt(t *testing.T) {
	p, work, deps, sessions, _ := retainedStepFixture(t)
	sessions.inspectErr = errors.New("node offline")
	_, err := runStepWithRecovery(t.Context(), p, work, nil, deps)
	if err == nil || !strings.Contains(err.Error(), "原") {
		t.Fatalf("missing actionable recovery diagnosis: %v", err)
	}
	records, _ := deps.Attempts.ForTask(t.Context(), p.TaskID)
	if len(records) != 1 || records[0].State != attempt.Running {
		t.Fatal("unreachable original step was replaced")
	}
}

func TestRetainedStepResumesVerificationWithoutRepeatingItsNativeInput(t *testing.T) {
	p, work, deps, sessions, tasks := retainedStepFixture(t, &plan.Verify{Kind: plan.VerifyAgent, Agent: "shipper"})
	calls := 0
	deps.Verifier = verifyFunc(func(StepRequest) error {
		calls++
		if calls == 1 {
			return agentexec.Blocked(attempt.Record{Spec: attempt.Spec{ID: "retained-verifier", TaskID: p.TaskID}}, "node", "检查原验证执行", "验证节点暂时离线。", "建议恢复验证节点后核对。", harness.ErrStopUnconfirmed)
		}
		return nil
	})
	_, err := runStepWithRecovery(t.Context(), p, work, nil, deps)
	var waiting *agentexec.RecoveryBlocked
	if !errors.As(err, &waiting) {
		t.Fatalf("verification interruption not preserved: %v", err)
	}
	records, err := deps.Attempts.ForTask(t.Context(), p.TaskID)
	if err != nil || len(records) != 1 || records[0].State != attempt.Verifying || records[0].Result == nil || len(records[0].Result.Output) == 0 || records[0].Unsettled {
		t.Fatalf("main step lost verification checkpoint or claimed verifier writer: %+v %v", records, err)
	}
	result, err := runStepWithRecovery(t.Context(), p, work, nil, deps)
	if err != nil || !result.Verified || result.Answer != sessions.answer {
		t.Fatalf("verification did not resume: %+v %v", result, err)
	}
	tracked, _ := tasks.Get(p.TaskID)
	if tracked.Budget.Turns != 1 {
		t.Fatal("resuming verification reserved main-step budget again")
	}
	sessions.mu.Lock()
	defer sessions.mu.Unlock()
	if sessions.prompted != 1 || sessions.resumed != 1 {
		t.Fatalf("native step was repeated for verification: %d/%d", sessions.prompted, sessions.resumed)
	}
}
