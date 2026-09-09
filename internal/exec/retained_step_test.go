package exec

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	"github.com/gopact-ai/steve/internal/lifecycle"
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
	// closeErr fails every close of the original session; closed counts
	// the attempts.
	closeErr error
	book     *ledger.Ledger
}

func (s *retainedStepSessions) ID() string { return "ns_step-original" }
func (s *retainedStepSessions) OpenSession(context.Context, harness.Placement, string, string, []acp.MCPServer) (harness.Runner, error) {
	return s, nil
}
func (s *retainedStepSessions) CloseSession(context.Context, harness.Placement, string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed++
	return s.closeErr
}
func (s *retainedStepSessions) closes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}
func (s *retainedStepSessions) failClose(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeErr = err
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

// retainedStepWorld is a plan whose one step runs on a node-owned session
// the test fakes, before anything has run.
func retainedStepWorld(t *testing.T, checks ...*plan.Verify) (plan.Plan, plan.Step, Deps, *retainedStepSessions, *task.Store) {
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
	registry := execution.New(t.Context(), tasks)
	deps := Deps{Executions: registry, Roster: fleet, Runner: runner, Budget: retainedStepBudget{tasks}, Workspaces: artifacts, Attempts: attempts, Artifacts: artifacts}
	p := plan.Plan{ID: "retained-plan", Rev: 1, TaskID: owner.ID, ProjectID: "p", Goal: owner.Goal}
	work := step("step-1", "work", []string{"gpu"})
	work.Agent = "builder"
	if len(checks) > 0 {
		work.Verify = checks[0]
	}
	return p, work, deps, sessions, tasks
}

// retainedStepFixture is retainedStepWorld after its step ran once and was
// retained on the node: the coordinator went away mid-prompt.
func retainedStepFixture(t *testing.T, checks ...*plan.Verify) (plan.Plan, plan.Step, Deps, *retainedStepSessions, *task.Store) {
	t.Helper()
	p, work, deps, sessions, tasks := retainedStepWorld(t, checks...)
	lifetime, stop := context.WithCancel(t.Context())
	registry := execution.New(lifetime, tasks)
	t.Cleanup(func() {
		stop()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = registry.Shutdown(ctx)
	})
	deps.Executions = registry
	attempts := deps.Attempts
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

// AttachStep finds the original command cancelled and settled on the node.
func (r settledFailedStep) AttachStep(ctx context.Context, req StepRequest, record attempt.Record) (RetainedStep, error) {
	joined, err := r.AgentRunner.AttachStep(ctx, req, record)
	if err != nil {
		return joined, err
	}
	joined.State.Command.Settled, joined.State.Command.State, joined.State.State = true, "cancelled", "idle"
	joined.Session = cancelledStep{r.sessions}
	return joined, nil
}

// cancelledStep is the original session whose command the node cancelled.
type cancelledStep struct{ *retainedStepSessions }

func (cancelledStep) ResumeTurn(context.Context, permission.AskFunc, acphost.AskUserFunc, func(view.Progress)) (string, []string, error) {
	return "", nil, harness.ErrTurnCanceled
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

// lostObserverStep joins a step whose node still runs the command, over a
// session whose resumed turn ends without settling.
type lostObserverStep struct {
	*AgentRunner
	sessions *retainedStepSessions
}

func (r lostObserverStep) AttachStep(ctx context.Context, req StepRequest, record attempt.Record) (RetainedStep, error) {
	joined, err := r.AgentRunner.AttachStep(ctx, req, record)
	if err != nil {
		return joined, err
	}
	joined.Session = lostStep{r.sessions}
	return joined, nil
}

type lostStep struct{ *retainedStepSessions }

func (lostStep) ResumeTurn(context.Context, permission.AskFunc, acphost.AskUserFunc, func(view.Progress)) (string, []string, error) {
	return "", nil, io.EOF
}

// TestRetainedStepRecoveryCodesReadAsBefore pins the code on the recovery
// question a joined step raises: a prompt the observer lost is the
// observer's (`observer`); a settled failure whose transition could not
// be written is the ledger's (`failure`).
func TestRetainedStepRecoveryCodesReadAsBefore(t *testing.T) {
	t.Run("observer", func(t *testing.T) {
		p, work, deps, sessions, _ := retainedStepFixture(t)
		deps.Runner = lostObserverStep{AgentRunner: deps.Runner.(*AgentRunner), sessions: sessions}
		_, err := runStepWithRecovery(t.Context(), p, work, nil, deps)
		var blocked *agentexec.RecoveryBlocked
		if !errors.As(err, &blocked) || !strings.HasSuffix(blocked.Question.RequestID, "/observer") || !errors.Is(err, io.EOF) {
			t.Fatalf("lost prompt on resume: %v", err)
		}
		records, err := deps.Attempts.ForTask(t.Context(), p.TaskID)
		if err != nil || len(records) != 1 || records[0].State != attempt.Running || sessions.closes() != 0 {
			t.Fatalf("lost observer settled the step: %+v %v closes=%d", records, err, sessions.closes())
		}
	})
	t.Run("failure", func(t *testing.T) {
		p, work, deps, sessions, _ := retainedStepFixture(t)
		deps.Runner = settledFailedStep{AgentRunner: deps.Runner.(*AgentRunner), sessions: sessions}
		cut := `CREATE TRIGGER cut BEFORE UPDATE OF state ON operations WHEN NEW.kind='attempt' AND NEW.state='failed' AND OLD.state!=NEW.state BEGIN SELECT RAISE(FAIL,'failure write unavailable'); END`
		if err := sessions.book.Update(t.Context(), func(tx *ledger.Tx) error { _, err := tx.Exec(cut); return err }); err != nil {
			t.Fatal(err)
		}
		_, err := runStepWithRecovery(t.Context(), p, work, nil, deps)
		var blocked *agentexec.RecoveryBlocked
		if !errors.As(err, &blocked) || !strings.HasSuffix(blocked.Question.RequestID, "/failure") {
			t.Fatalf("failure transition cut on resume: %v", err)
		}
		records, err := deps.Attempts.ForTask(t.Context(), p.TaskID)
		if err != nil || len(records) != 1 || records[0].State != attempt.Running || sessions.closes() != 0 {
			t.Fatalf("unwritten failure settled the step: %+v %v closes=%d", records, err, sessions.closes())
		}
	})
}

// stoppedSettledStep joins a step whose node finished the command while
// the task was set aside in the same turn.
type stoppedSettledStep struct {
	*AgentRunner
	sessions *retainedStepSessions
}

func (r stoppedSettledStep) AttachStep(ctx context.Context, req StepRequest, record attempt.Record) (RetainedStep, error) {
	joined, err := r.AgentRunner.AttachStep(ctx, req, record)
	if err != nil {
		return joined, err
	}
	joined.Session = pausedStep{r.sessions}
	return joined, nil
}

// pausedStep is a node-owned session whose turn settles well after the
// task was paused: live, from the prompt; joined, from the resumed turn.
type pausedStep struct{ *retainedStepSessions }

func (s pausedStep) pause(ctx context.Context) (string, []string, error) {
	records, err := s.attempts.Live(ctx)
	if err != nil || len(records) != 1 {
		return "", nil, errors.New("one original step required")
	}
	if _, err := s.tasks.SetAside(records[0].TaskID, task.StatePaused); err != nil {
		return "", nil, err
	}
	return s.answer, nil, nil
}
func (s pausedStep) OpenSession(context.Context, harness.Placement, string, string, []acp.MCPServer) (harness.Runner, error) {
	return s, nil
}
func (s pausedStep) Prompt(ctx context.Context, _ string, _ func(view.Progress)) (string, []string, error) {
	return s.pause(ctx)
}
func (s pausedStep) ResumeTurn(ctx context.Context, _ permission.AskFunc, _ acphost.AskUserFunc, _ func(view.Progress)) (string, []string, error) {
	return s.pause(ctx)
}

// TestStoppedSettledStepWhoseFailureCannotBeWrittenIsAFailure pins a prompt
// that settled well in the turn the task was stopped in: the step fails as
// a cancelled turn, and when the failure cannot be written the question is
// the ledger's (`failure`), carrying the cancelled turn as the step's
// error — on the live path and the joined one alike.
func TestStoppedSettledStepWhoseFailureCannotBeWrittenIsAFailure(t *testing.T) {
	cut := `CREATE TRIGGER cut BEFORE UPDATE OF state ON operations WHEN NEW.kind='attempt' AND NEW.state='failed' AND OLD.state!=NEW.state BEGIN SELECT RAISE(FAIL,'failure write unavailable'); END`
	check := func(t *testing.T, p plan.Plan, deps Deps, sessions *retainedStepSessions, result plan.StepResult, err error) {
		t.Helper()
		var blocked *agentexec.RecoveryBlocked
		if !errors.As(err, &blocked) || !strings.HasSuffix(blocked.Question.RequestID, "/failure") {
			t.Fatalf("failure transition cut after a stop: %v", err)
		}
		if result.Error != harness.ErrTurnCanceled.Error() {
			t.Fatalf("step error = %q, want the cancelled turn", result.Error)
		}
		records, err := deps.Attempts.ForTask(t.Context(), p.TaskID)
		if err != nil || len(records) != 1 || records[0].State != attempt.Running || sessions.closes() != 0 {
			t.Fatalf("unwritten failure settled the step: %+v %v closes=%d", records, err, sessions.closes())
		}
	}
	t.Run("live", func(t *testing.T) {
		p, work, deps, sessions, _ := retainedStepWorld(t)
		deps.Runner = NewAgentRunner(pausedStep{sessions}, nil, deps.Roster)
		if err := sessions.book.Update(t.Context(), func(tx *ledger.Tx) error { _, err := tx.Exec(cut); return err }); err != nil {
			t.Fatal(err)
		}
		result, err := runStepWithRecovery(t.Context(), p, work, nil, deps)
		check(t, p, deps, sessions, result, err)
	})
	t.Run("joined", func(t *testing.T) {
		p, work, deps, sessions, _ := retainedStepFixture(t)
		deps.Runner = stoppedSettledStep{AgentRunner: deps.Runner.(*AgentRunner), sessions: sessions}
		if err := sessions.book.Update(t.Context(), func(tx *ledger.Tx) error { _, err := tx.Exec(cut); return err }); err != nil {
			t.Fatal(err)
		}
		result, err := runStepWithRecovery(t.Context(), p, work, nil, deps)
		check(t, p, deps, sessions, result, err)
	})
}

// failingBudget settles no attempt.
type failingBudget struct{ err error }

func (failingBudget) Reserve(string) (int, time.Time, error) { return 0, time.Time{}, nil }
func (failingBudget) ReserveAttempt(attempt.Record) (int, time.Time, error) {
	return 0, time.Time{}, nil
}
func (b failingBudget) SettleAttempt(attempt.Record, task.Outcome) error { return b.err }

// TestStepSettlementBlocksAsBefore pins how a step reports a settlement it
// could not finish — its budget, its node session — on each path it had
// before Run: a prompt's own failure carries the block as a `failure`
// question (in the live path's words, or the joined path's); a finish that
// refused the work returns its cause and leaves the block to the execution
// scope; a bound step's budget is its own question, and its unreleased
// session is delivered past.
func TestStepSettlementBlocksAsBefore(t *testing.T) {
	prompt, refusal := errors.New("the agent gave up"), errors.New("wrote outside its declared paths")
	closeErr := errors.Join(harness.ErrStopUnconfirmed, errors.New("node away"))
	failed := attempt.Record{Spec: attempt.Spec{ID: "a1", TaskID: "t", Node: "n1"}, State: attempt.Failed, Session: "ns_1"}
	bound := failed
	bound.State = attempt.Bound
	for _, tc := range []struct {
		name            string
		joined, refused bool
		record          attempt.Record
		budget, cleanup error
		cause           error
		code            string // on the returned question; "" returns the cause itself
		scope           string // on the scope's block; "" leaves nothing unresolved
		detached        bool   // the scope's block is a detached observer's
	}{
		{name: "live prompt failure, accounting", record: failed, budget: errors.New("ledger away"), cause: prompt, code: "/failure", scope: "/accounting"},
		{name: "live prompt failure, cleanup", record: failed, cleanup: closeErr, cause: prompt, code: "/failure", scope: "/cleanup"},
		{name: "live refusal, accounting", refused: true, record: failed, budget: errors.New("ledger away"), cause: refusal, scope: "/accounting"},
		{name: "live refusal, cleanup", refused: true, record: failed, cleanup: closeErr, cause: refusal, scope: "/cleanup"},
		{name: "joined prompt failure, accounting", joined: true, record: failed, budget: errors.New("ledger away"), cause: prompt, code: "/failure", scope: "/accounting", detached: true},
		{name: "joined prompt failure, cleanup", joined: true, record: failed, cleanup: closeErr, cause: prompt, code: "/failure", scope: "/cleanup", detached: true},
		{name: "joined refusal, cleanup", joined: true, refused: true, record: failed, cleanup: closeErr, cause: refusal, scope: "/cleanup"},
		{name: "bound, accounting", record: bound, budget: errors.New("ledger away"), code: "/accounting"},
		{name: "bound, cleanup", record: bound, cleanup: closeErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &stepRun{deps: Deps{Budget: failingBudget{tc.budget}}, record: tc.record, reserved: true, joined: tc.joined, refused: tc.refused, managed: true}
			prompt := tc.cause
			if tc.refused {
				prompt = nil
			}
			_, err, unresolved := r.settle(lifecycle.Result{Record: tc.record, Driven: true, Managed: true, Err: prompt, CleanupErr: tc.cleanup}, tc.cause)
			var blocked *agentexec.RecoveryBlocked
			switch {
			case tc.code == "" && err != tc.cause:
				t.Fatalf("err = %v, want the cause %v", err, tc.cause)
			case tc.code != "" && (!errors.As(err, &blocked) || !strings.HasSuffix(blocked.Question.RequestID, tc.code)):
				t.Fatalf("err = %v, want a %s question", err, tc.code)
			}
			var inScope *agentexec.RecoveryBlocked
			var observer *execution.RetainedObserverDetached
			switch {
			case tc.scope == "" && unresolved != nil:
				t.Fatalf("unresolved = %v, want nothing", unresolved)
			case tc.scope != "" && (!errors.As(unresolved, &inScope) || !strings.HasSuffix(inScope.Question.RequestID, tc.scope)):
				t.Fatalf("unresolved = %v, want a %s block", unresolved, tc.scope)
			case errors.As(unresolved, &observer) != tc.detached:
				t.Fatalf("unresolved = %v, detached observer = %v", unresolved, tc.detached)
			}
			if tc.code != "" && tc.scope != "" && !errors.Is(err, inScope) {
				t.Fatalf("the question %v does not carry the block %v", err, inScope)
			}
		})
	}
}

// A completion deferred to another execution's recovery — a verifier whose
// own retained attempt an observer elsewhere is joining — hands back that
// recovery's block and leaves nothing unresolved in the step's own scope.
// The step's session was closed before the finish judged the work, so no
// writer of its own is unaccounted for; the verifier's block is the
// verifier's scope's to keep. The record stays in the phase it reached, for
// the observer that comes back to resume from; a block on this attempt's
// own recovery is the step's to report.
func TestDeferredStepLeavesNothingUnresolvedOfItsOwn(t *testing.T) {
	verifier := attempt.Record{Spec: attempt.Spec{ID: "v1", TaskID: "t", Node: "n1"}, State: attempt.Running, Session: "ns_v"}
	waiting := agentexec.Blocked(verifier, "observer", "连接原验证的节点", "原验证暂时不能安全接续。", "建议恢复原节点后重新检查。", nil)
	record := attempt.Record{Spec: attempt.Spec{ID: "a1", TaskID: "t", Node: "n1"}, State: attempt.Verifying, Session: "ns_1"}
	r := &stepRun{record: record, managed: true, closed: true}
	deferred := r.blocked("verify", waiting)
	var d *lifecycle.Deferred
	if !errors.As(deferred, &d) || d.Cause != waiting {
		t.Fatalf("another execution's block = %v, want a deferral on it", deferred)
	}
	result, err, unresolved := r.settle(lifecycle.Result{Record: record, Driven: true, Managed: true}, deferred)
	if err != waiting || unresolved != nil || r.record.State != attempt.Verifying {
		t.Fatalf("deferred step: result=%+v err=%v unresolved=%v state=%s", result, err, unresolved, r.record.State)
	}
	own := agentexec.Blocked(record, "observer", "连接原步骤的节点", "原步骤暂时不能安全接续。", "建议恢复原节点后重新检查。", nil)
	if err := r.blocked("verify", own); err != own {
		t.Fatalf("this attempt's own block = %v, want it as it is", err)
	}
}

// TestRetainedFailedStepWhoseCloseFailsIsCleanedUpOnRestore: the failure is
// on record and its budget settled, but the node did not confirm the
// session closed. The record is not quarantined — restoring the step
// closes the session again, and blocks only until that succeeds.
func TestRetainedFailedStepWhoseCloseFailsIsCleanedUpOnRestore(t *testing.T) {
	p, work, deps, sessions, tasks := retainedStepFixture(t)
	deps.Runner = settledFailedStep{AgentRunner: deps.Runner.(*AgentRunner), sessions: sessions}
	sessions.failClose(errors.New("node away"))
	_, err := runStepWithRecovery(t.Context(), p, work, nil, deps)
	var blocked *agentexec.RecoveryBlocked
	if !errors.As(err, &blocked) || !strings.HasSuffix(blocked.Question.RequestID, "/failure") || !errors.Is(err, harness.ErrStopUnconfirmed) {
		t.Fatalf("unconfirmed close after a failed prompt did not ask as before: %v", err)
	}
	records, err := deps.Attempts.ForTask(t.Context(), p.TaskID)
	if err != nil || len(records) != 1 || records[0].State != attempt.Failed || records[0].Unsettled || sessions.closes() != 1 {
		t.Fatalf("failed step with an unconfirmed close: records=%+v err=%v closes=%d", records, err, sessions.closes())
	}
	if tracked, _ := tasks.Get(p.TaskID); tracked.Attempts[0].Open() {
		t.Fatal("failed step kept its accounting open behind an unconfirmed close")
	}
	// The next restore closes it again: blocked while the node is away,
	// clean once it answers, and never a stop confirmation.
	restored := work
	if _, ok, err := restoreStep(t.Context(), p, &restored, nil, deps); ok || !errors.As(err, &blocked) || !strings.HasSuffix(blocked.Question.RequestID, "/cleanup") {
		t.Fatalf("restore with the node away: ok=%v err=%v", ok, err)
	}
	sessions.failClose(nil)
	if _, ok, err := restoreStep(t.Context(), p, &restored, nil, deps); ok || err != nil || sessions.closes() != 3 {
		t.Fatalf("restore once the node answers: ok=%v err=%v closes=%d", ok, err, sessions.closes())
	}
	if _, ok, err := restoreStep(t.Context(), p, &restored, nil, deps); ok || err != nil {
		t.Fatalf("restore after cleanup: ok=%v err=%v", ok, err)
	}
	if record, err := deps.Attempts.Get(t.Context(), records[0].ID); err != nil || record.State != attempt.Failed || record.Unsettled {
		t.Fatalf("cleaned-up step: %+v %v", record, err)
	}
}

// TestRetainedBoundStepWhoseCloseFailsStillDeliversItsResult: a step
// resumed past its prompt is committed, and its close fails afterwards.
// The result is on record and delivered; only the session and the
// workspace wait for the node.
func TestRetainedBoundStepWhoseCloseFailsStillDeliversItsResult(t *testing.T) {
	p, work, deps, sessions, tasks := retainedStepFixture(t, &plan.Verify{Kind: plan.VerifyAgent, Agent: "shipper"})
	deps.Verifier = verifyFunc(func(StepRequest) error { return nil })
	cut := `CREATE TRIGGER cut BEFORE UPDATE OF state ON operations WHEN NEW.kind='attempt' AND NEW.state='bound' AND OLD.state!=NEW.state BEGIN SELECT RAISE(FAIL,'phase write unavailable'); END`
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
	sessions.failClose(errors.New("node away"))
	result, err := runStepWithRecovery(t.Context(), p, work, nil, deps)
	if err != nil || result.Answer != sessions.answer || !result.Verified || result.AttemptID == "" {
		t.Fatalf("bound result withheld behind an unconfirmed close: %+v %v", result, err)
	}
	records, err := deps.Attempts.ForTask(t.Context(), p.TaskID)
	if err != nil || len(records) != 1 || records[0].State != attempt.Bound || records[0].Unsettled || sessions.closes() != 2 {
		t.Fatalf("bound step with an unconfirmed close: records=%+v err=%v closes=%d", records, err, sessions.closes())
	}
	if tracked, _ := tasks.Get(p.TaskID); tracked.Budget.Turns != 1 || tracked.Attempts[0].Open() {
		t.Fatalf("bound step's accounting: %+v", tracked)
	}
	// The committed result restores as any other; the close is not tried
	// again on a step that is done.
	again, err := runStepWithRecovery(t.Context(), p, work, nil, deps)
	if err != nil || again.AttemptID != result.AttemptID || sessions.closes() != 2 || sessions.resumed != 1 {
		t.Fatalf("bound step restored: %+v %v closes=%d resumes=%d", again, err, sessions.closes(), sessions.resumed)
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
