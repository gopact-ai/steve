package agentexec

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/ability"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/roster"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/view"
)

type testBudget struct{ tasks *task.Store }

func (b testBudget) Reserve(id string) (int, time.Time, error) {
	_, err := b.tasks.ReserveTurn(id)
	return 0, time.Time{}, err
}

func (b testBudget) ReserveAttempt(record attempt.Record) (int, time.Time, error) {
	_, err := b.tasks.ReserveAttempt(*record.Execution, record.ID, record.TurnID, record.Agent, record.Node, record.StartedAt)
	return 0, time.Time{}, err
}

func (b testBudget) SettleAttempt(record attempt.Record, outcome task.Outcome) error {
	var usage task.RecoveryUsage
	if u := record.Usage; u != nil {
		usage = task.RecoveryUsage{Tokens: task.Tokens{Input: u.Input, Output: u.Output, CachedRead: u.CachedRead, CachedWrite: u.CachedWrite}, Model: u.Model, Reported: u.Reported}
	}
	return b.tasks.SettleAttempt(record.TaskID, record.ID, record.TurnID, record.EndedAt, outcome, usage)
}

type testSessions struct {
	stopped        bool
	opened, closed atomic.Int32
	aborted        atomic.Int32
	run            func(context.Context, string, func(view.Progress)) (string, error)
	openErr        error
	closeErr       error
}

func (s *testSessions) OpenSession(context.Context, harness.Placement, string, string, []acp.MCPServer) (harness.Runner, error) {
	if s.openErr != nil {
		return nil, s.openErr
	}
	s.opened.Add(1)
	return &testSession{owner: s}, nil
}
func (s *testSessions) CloseSession(context.Context, harness.Placement, string) error {
	s.closed.Add(1)
	return s.closeErr
}

type testSession struct{ owner *testSessions }

func (*testSession) ID() string { return "test" }
func (s *testSession) Prompt(ctx context.Context, prompt string, progress func(view.Progress)) (string, []string, error) {
	if s.owner.run != nil {
		answer, err := s.owner.run(ctx, prompt, progress)
		return answer, nil, err
	}
	progress(view.Progress{Settings: view.Settings{Model: "model"}, Usage: view.Usage{Reported: true, InputTokens: 100, OutputTokens: 20, ContextTokens: 500}})
	return "PASS", nil, nil
}
func (*testSession) Cancel(context.Context) error { return nil }
func (s *testSession) Abort()                     { s.owner.aborted.Add(1) }
func (s *testSession) Stopped() bool              { return s.owner.stopped }

type testWorld struct {
	book     *ledger.Ledger
	attempts *attempt.Service
	tasks    *task.Store
	runner   *Runner
	sessions *testSessions
	work     task.Task
	catalog  *agent.Catalog
}

func world(t *testing.T, slots int) *testWorld {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	projects := project.Open(book)
	if err := projects.Declare(t.Context(), []project.Project{{ID: "p", Home: project.Home{Path: t.TempDir()}}}); err != nil {
		t.Fatal(err)
	}
	tasks, err := task.OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	work, err := tasks.Create(task.Task{Member: "agent", ProjectID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := agent.NewCatalog(map[string]agent.Config{"agent": {Harness: "mock", Default: true}})
	if err != nil {
		t.Fatal(err)
	}
	fleet := roster.New(catalog)
	fleet.SetHubCapabilities([]string{"basic"})
	fleet.SetHubSlots(map[string]int{"mock": slots})
	att := attempt.New(book)
	sessions := &testSessions{}
	runner := New(sessions, fleet, artifact.New(t.TempDir(), book, projects, artifact.LocalNodes{Dir: t.TempDir()}), att, execution.New(t.Context(), tasks), testBudget{tasks})
	runner.slotPoll = 5 * time.Millisecond
	return &testWorld{book: book, attempts: att, tasks: tasks, runner: runner, sessions: sessions, work: work, catalog: catalog}
}
func (w *testWorld) spec(kind attempt.Kind) Spec {
	return Spec{TaskID: w.work.ID, TurnID: "test", Agent: "agent", Project: "p", Kind: kind}
}

func TestAuxiliaryPromptsUseIndependentAttemptsAndAccountUsage(t *testing.T) {
	w := world(t, 0)
	var paths []string
	for _, kind := range []attempt.Kind{attempt.KindPlan, attempt.KindVerify} {
		out, err := w.runner.Prompt(t.Context(), w.spec(kind), "work", nil)
		if err != nil {
			t.Fatal(err)
		}
		if out.Attempt.Kind != kind || out.Attempt.State != attempt.Bound || out.Attempt.Execution == nil || out.Usage == nil || !out.Usage.Reported || out.Usage.Input != 100 || out.Usage.Context != 500 {
			t.Fatalf("out=%+v", out)
		}
		if out.Attempt.Workspace.Kind != project.KindWorktree {
			t.Fatal("not isolated")
		}
		paths = append(paths, out.Attempt.Workspace.Path)
		if _, err := os.Stat(out.Attempt.Workspace.Path); !os.IsNotExist(err) {
			t.Fatalf("completed workspace not discarded: %v", err)
		}
	}
	if paths[0] == paths[1] || w.sessions.closed.Load() != 2 {
		t.Fatal("attempt workspace or session ownership reused")
	}
	work, _ := w.tasks.Get(w.work.ID)
	if work.Budget.Turns != 2 || len(work.Attempts) != 2 || work.Budget.Tokens.Total != 240 || work.Attempts[0].Open() || work.Attempts[1].Open() {
		t.Fatalf("budget=%+v", work)
	}
}

func TestAuxiliaryValidationAndPromptFailuresStillSettle(t *testing.T) {
	for _, stage := range []string{"open", "prompt", "validation"} {
		t.Run(stage, func(t *testing.T) {
			w := world(t, 0)
			cause := errors.New(stage + " failed")
			validate := func(string) error { return nil }
			if stage == "open" {
				w.sessions.openErr = cause
			}
			if stage == "prompt" {
				w.sessions.stopped = true
				w.sessions.run = func(_ context.Context, _ string, progress func(view.Progress)) (string, error) {
					progress(view.Progress{Usage: view.Usage{Reported: true, InputTokens: 120}})
					return "partial", cause
				}
			}
			if stage == "validation" {
				validate = func(string) error { return cause }
			}
			out, err := w.runner.Prompt(t.Context(), w.spec(attempt.KindPlan), "work", validate)
			if !errors.Is(err, cause) || out.Attempt.State != attempt.Failed {
				t.Fatalf("out=%+v err=%v", out, err)
			}
			if errors.Is(err, context.Canceled) {
				t.Fatalf("cleanup mislabeled the failure as canceled: %v", err)
			}
			var invalid *ValidationError
			if errors.As(err, &invalid) != (stage == "validation") {
				t.Fatalf("incorrect correction eligibility: %v", err)
			}
			if stage != "open" && (out.Attempt.Usage == nil || !out.Attempt.Usage.Reported) {
				t.Fatal("failed execution lost usage")
			}
			if _, err := os.Stat(out.Attempt.Workspace.Path); !os.IsNotExist(err) {
				t.Fatalf("failed workspace remains: %v", err)
			}
		})
	}
}

func TestAuxiliarySlotAdmissionWaitsAndDoesNotSpendBeforeEntry(t *testing.T) {
	w := world(t, 1)
	held, err := w.attempts.Open(t.Context(), attempt.Spec{ID: "held", TaskID: w.work.ID, Agent: "agent", Harness: "mock", Slots: 1, Scope: attempt.ScopeNone, Workspace: project.Workspace{ID: "held", Path: t.TempDir(), Kind: project.KindWorktree}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err = w.runner.Prompt(ctx, w.spec(attempt.KindVerify), "work", nil)
	if !errors.Is(err, context.DeadlineExceeded) || w.sessions.opened.Load() != 0 {
		t.Fatalf("slot bypassed: opened=%d err=%v", w.sessions.opened.Load(), err)
	}
	work, _ := w.tasks.Get(w.work.ID)
	if work.Budget.Turns != 0 {
		t.Fatal("waiting invocation spent a turn")
	}
	if _, err := w.attempts.FailWith(t.Context(), held.ID, "test", "release", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := w.runner.Prompt(t.Context(), w.spec(attempt.KindVerify), "work", nil); err != nil {
		t.Fatal(err)
	}
}

type interruptedWorkspaces struct {
	Workspaces
	cancel context.CancelFunc
	err    error
}

func (w interruptedWorkspaces) Materialize(ctx context.Context, _ project.Request) (project.Workspace, error) {
	if w.cancel != nil {
		w.cancel()
	}
	<-ctx.Done()
	return project.Workspace{}, w.err
}

func TestAuxiliaryPreparationPreservesCancellationAndDependencyError(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "canceled", true: "deadline"}[deadline], func(t *testing.T) {
			w := world(t, 1)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			cause := errors.New("dependency interrupted")
			workspaces := interruptedWorkspaces{Workspaces: w.runner.workspaces, cancel: cancel, err: cause}
			spec := w.spec(attempt.KindVerify)
			want := context.Canceled
			if deadline {
				workspaces.cancel = nil
				spec.Timeout = time.Second
				want = context.DeadlineExceeded
			}
			w.runner.workspaces = workspaces
			_, err := w.runner.Prompt(ctx, spec, "work", nil)
			if !errors.Is(err, want) || !errors.Is(err, cause) {
				t.Fatalf("cancellation or dependency failure lost: %v", err)
			}
			work, ok := w.tasks.Get(w.work.ID)
			if !ok {
				t.Fatal("task disappeared")
			}
			if w.sessions.opened.Load() != 0 || work.Budget.Turns != 0 {
				t.Fatalf("canceled preparation executed or spent budget: opened=%d turns=%d", w.sessions.opened.Load(), work.Budget.Turns)
			}
		})
	}
}

func TestUnconfirmedAuxiliaryWriterIsQuarantinedAtItsOwnAttempt(t *testing.T) {
	w := world(t, 1)
	w.sessions.run = func(_ context.Context, _ string, progress func(view.Progress)) (string, error) {
		progress(view.Progress{Usage: view.Usage{Reported: true, InputTokens: 20}})
		return "partial", harness.ErrStopUnconfirmed
	}
	out, err := w.runner.Prompt(t.Context(), w.spec(attempt.KindVerify), "work", nil)
	var unsettled *UnsettledError
	if !errors.Is(err, harness.ErrStopUnconfirmed) || !errors.As(err, &unsettled) || unsettled.UnsettledAttempt() != out.Attempt.ID || !out.Attempt.Unsettled {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if w.sessions.closed.Load() != 0 {
		t.Fatal("closed an unconfirmed writer")
	}
	if _, err := os.Stat(out.Attempt.Workspace.Path); err != nil {
		t.Fatal("discarded quarantined workspace", err)
	}
	wait := w.runner.executions.Stop([]string{w.work.ID}, context.Canceled)
	if err := wait.Wait(t.Context()); !errors.Is(err, harness.ErrStopUnconfirmed) {
		t.Fatalf("scope falsely claimed quiescence: %v", err)
	}
	if _, err := w.runner.Prompt(t.Context(), w.spec(attempt.KindPlan), "another", nil); err == nil {
		t.Fatal("finite endpoint bypassed quarantine")
	}
}

func TestCloseFailureDoesNotKillOtherSessionsAndCannotClaimSuccess(t *testing.T) {
	w := world(t, 1)
	w.sessions.closeErr = errors.New("close was not acknowledged")
	out, err := w.runner.Prompt(t.Context(), w.spec(attempt.KindVerify), "work", nil)
	if !errors.Is(err, harness.ErrStopUnconfirmed) || !out.Attempt.Unsettled || w.sessions.aborted.Load() != 0 {
		t.Fatalf("close failure lost quarantine or killed shared host: out=%+v aborts=%d err=%v", out, w.sessions.aborted.Load(), err)
	}
	if _, err := os.Stat(out.Attempt.Workspace.Path); err != nil {
		t.Fatal("close failure discarded real workspace", err)
	}
}

type deadlineBudget struct{ Budget }

func (b deadlineBudget) Reserve(id string) (int, time.Time, error) {
	left, _, err := b.Budget.Reserve(id)
	return left, time.Now().Add(time.Minute), err
}
func TestBudgetDeadlineDoesNotCancelSuccessfulSettlement(t *testing.T) {
	w := world(t, 0)
	w.runner.budget = deadlineBudget{w.runner.budget}
	out, err := w.runner.Prompt(t.Context(), w.spec(attempt.KindPlan), "work", nil)
	if err != nil || out.Attempt.State != attempt.Bound {
		t.Fatalf("cleanup cancellation overrode a completed prompt: out=%+v err=%v", out, err)
	}
}

func TestTaskCancellationReachesAuxiliaryScopeAndPreventsSuccess(t *testing.T) {
	w := world(t, 0)
	entered := make(chan struct{})
	w.sessions.run = func(ctx context.Context, _ string, progress func(view.Progress)) (string, error) {
		close(entered)
		<-ctx.Done()
		progress(view.Progress{Usage: view.Usage{Reported: true}})
		return "late success", nil
	}
	var out Result
	var err error
	var wg sync.WaitGroup
	wg.Go(func() { out, err = w.runner.Prompt(t.Context(), w.spec(attempt.KindPlan), "work", nil) })
	<-entered
	ids, stopErr := w.tasks.SetAside(w.work.ID, task.StateCancelled)
	if stopErr != nil {
		t.Fatal(stopErr)
	}
	wait := w.runner.executions.Stop(ids, task.ErrExecutionStopped)
	wg.Wait()
	if err == nil || out.Attempt.State != attempt.Failed || out.Usage == nil || !out.Usage.Reported {
		t.Fatalf("cancelled auxiliary succeeded: %+v err=%v", out, err)
	}
	if err := wait.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(out.Attempt.Workspace.Path); !os.IsNotExist(err) {
		t.Fatal("confirmed cancelled workspace not discarded")
	}
}

func TestAuxiliaryRequiresFreshAdmissionBeforeOpeningSession(t *testing.T) {
	w := world(t, 0)
	if err := w.catalog.Set("agent", agent.Config{Harness: "mock", Requires: []string{"mcp:missing"}, Default: true}); err != nil {
		t.Fatal(err)
	}
	_, err := w.runner.Prompt(t.Context(), w.spec(attempt.KindPlan), "work", nil)
	if err == nil || w.sessions.opened.Load() != 0 {
		t.Fatal("unavailable requirement launched session")
	}
}

type refusingNode struct {
	calls   int
	verdict ability.Verdict
}

func (n *refusingNode) Statuses() []node.Status {
	return []node.Status{{Name: "remote", Up: true, Advert: nodewire.Advert{Node: "remote", Capabilities: []string{"gpu"}, Harnesses: []nodewire.Harness{{ID: "mock"}}}}}
}
func (*refusingNode) EnsureConnected(context.Context, ...string) {}
func (n *refusingNode) Admit(context.Context, string, nodewire.AdmitRequest) (ability.Admission, error) {
	n.calls++
	return ability.Admission{Verdict: n.verdict, Source: ability.SourceNode}, nil
}
func (*refusingNode) Bindings(context.Context, string, string) []ability.Binding { return nil }
func (*refusingNode) Release(context.Context, string, string) error              { return nil }

func TestAuxiliaryFreshAdmissionCanOverrideRosterEligibility(t *testing.T) {
	for _, verdict := range []ability.Verdict{ability.False, ability.Unsure} {
		w := world(t, 0)
		remote := &refusingNode{verdict: verdict}
		w.runner.roster.SetNodes(remote)
		if err := w.catalog.Set("agent", agent.Config{Harness: "mock", Node: "remote", Requires: []string{"gpu"}, Default: true}); err != nil {
			t.Fatal(err)
		}
		if !w.runner.roster.All(t.Context())[0].Eligible {
			t.Fatal("fixture was not eligible before fresh admission")
		}
		out, err := w.runner.Prompt(t.Context(), w.spec(attempt.KindPlan), "work", nil)
		if err == nil || remote.calls != 1 || w.sessions.opened.Load() != 0 || out.Attempt.State != attempt.Failed {
			t.Fatalf("fresh admission bypassed: out=%+v calls=%d opened=%d err=%v", out, remote.calls, w.sessions.opened.Load(), err)
		}
	}
}
