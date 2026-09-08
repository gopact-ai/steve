package agentexec

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/roster"
	"github.com/gopact-ai/steve/internal/view"
)

type retainedAuxNodes struct{}

func (retainedAuxNodes) Statuses() []node.Status {
	return []node.Status{{Name: "worker", Up: true, Advert: nodewire.Advert{Node: "worker", Harnesses: []nodewire.Harness{{ID: "mock"}}}}}
}
func (retainedAuxNodes) EnsureConnected(context.Context, ...string) {}

type retainedAuxSessions struct {
	mu                        sync.Mutex
	w                         *testWorld
	entered                   chan struct{}
	prompted, resumed, closed int
	answer                    string
	inspectErr                error
}

func (s *retainedAuxSessions) ID() string { return "ns_aux-original" }
func (s *retainedAuxSessions) OpenSession(context.Context, harness.Placement, string, string, []acp.MCPServer) (harness.Runner, error) {
	return s, nil
}
func (s *retainedAuxSessions) CloseSession(context.Context, harness.Placement, string) error {
	s.mu.Lock()
	s.closed++
	s.mu.Unlock()
	return nil
}
func (s *retainedAuxSessions) AttachRetainedSession(context.Context, harness.Placement, string, string) (harness.ResumableRunner, error) {
	return s, nil
}
func (s *retainedAuxSessions) Cancel(context.Context) error { return nil }
func (s *retainedAuxSessions) Abort()                       {}
func (s *retainedAuxSessions) Prompt(ctx context.Context, _ string, p func(view.Progress)) (string, []string, error) {
	s.mu.Lock()
	s.prompted++
	s.mu.Unlock()
	close(s.entered)
	<-ctx.Done()
	return "", nil, errors.Join(harness.ErrStopUnconfirmed, ctx.Err())
}
func (s *retainedAuxSessions) InspectRetained(ctx context.Context) (nodewire.SessionState, error) {
	if s.inspectErr != nil {
		return nodewire.SessionState{}, s.inspectErr
	}
	list, err := s.w.attempts.Live(ctx)
	if err != nil || len(list) != 1 {
		return nodewire.SessionState{}, errors.New("one original attempt required")
	}
	r := list[0]
	state := nodewire.SessionState{ID: s.ID(), Harness: r.Harness, State: "running", InputAccepted: 1, Binding: nodewire.SessionBinding{ProjectID: r.Project, SessionID: attempt.RetainedSessionID("", r.TaskID, r.Agent), TaskID: r.TaskID, AttemptID: r.ID, NodeID: r.Node, TaskEpoch: r.Execution.Epoch, ExecutionEpoch: attempt.SessionExecutionEpoch(r)}, Command: &nodewire.SessionCommand{ID: attempt.InputCommandID(r), InputSequence: 1, State: "running"}}
	if r.SessionSettled != nil && *r.SessionSettled {
		state.State, state.Command.State, state.Command.Settled = "idle", "completed", true
		state.Command.Output = s.answer
	}
	return state, nil
}

func TestRetainedAuxiliaryRebuildsParsedOutputAfterCompletionWriteFailure(t *testing.T) {
	w, sessions, spec := retainedAuxFixture(t)
	next := New(sessions, w.runner.roster, w.runner.workspaces, w.attempts, execution.New(t.Context(), w.tasks), w.runner.budget)
	if err := w.book.Update(t.Context(), func(tx *ledger.Tx) error {
		_, err := tx.Exec(`CREATE TRIGGER reject_bound BEFORE UPDATE OF state ON operations WHEN NEW.kind='attempt' AND NEW.state='bound' BEGIN SELECT RAISE(FAIL,'completion unavailable'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	parsed := ""
	validate := func(answer string) error { parsed = answer; return nil }
	_, firstErr := next.Prompt(t.Context(), spec, "verify original work", validate)
	if firstErr == nil {
		t.Fatal("bound write failure was ignored")
	}
	if parsed != sessions.answer {
		t.Fatalf("initial retained output was not parsed: %v", firstErr)
	}
	if err := w.book.Update(t.Context(), func(tx *ledger.Tx) error { _, err := tx.Exec("DROP TRIGGER reject_bound"); return err }); err != nil {
		t.Fatal(err)
	}
	parsed = ""
	out, err := next.Prompt(t.Context(), spec, "verify original work", validate)
	if err != nil || out.Attempt.State != attempt.Bound || parsed != sessions.answer || sessions.prompted != 1 || sessions.resumed != 1 {
		t.Fatalf("saved output was not rebuilt without native replay: out=%+v parsed=%q native=%d/%d err=%v", out, parsed, sessions.prompted, sessions.resumed, err)
	}
}
func (s *retainedAuxSessions) ResumeTurn(_ context.Context, _ permission.AskFunc, _ acphost.AskUserFunc, p func(view.Progress)) (string, []string, error) {
	s.mu.Lock()
	s.resumed++
	s.mu.Unlock()
	if p != nil {
		p(view.Progress{Settings: view.Settings{Model: "original-model"}, Usage: view.Usage{Reported: true, InputTokens: 17, OutputTokens: 3}})
	}
	return s.answer, nil, nil
}
func retainedAuxFixture(t *testing.T) (*testWorld, *retainedAuxSessions, Spec) {
	t.Helper()
	w := world(t, 0)
	catalog, err := agent.NewCatalog(map[string]agent.Config{"agent": {Harness: "mock", Node: "worker", Default: true}})
	if err != nil {
		t.Fatal(err)
	}
	fleet := roster.New(catalog)
	fleet.SetNodes(retainedAuxNodes{})
	w.runner.roster = fleet
	sessions := &retainedAuxSessions{w: w, entered: make(chan struct{}), answer: "  PASS\ncomplete original output\n"}
	w.runner.sessions = sessions
	lifetime, stop := context.WithCancel(t.Context())
	w.runner.executions = execution.New(lifetime, w.tasks)
	t.Cleanup(func() {
		stop()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = w.runner.executions.Shutdown(ctx)
	})
	spec := w.spec(attempt.KindVerify)
	done := make(chan error, 1)
	go func() { _, err := w.runner.Prompt(lifetime, spec, "verify original work", nil); done <- err }()
	select {
	case <-sessions.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("auxiliary did not start")
	}
	records, err := w.attempts.Live(t.Context())
	if err != nil || len(records) != 1 || records[0].Session != sessions.ID() {
		stop()
		<-done
		t.Fatalf("native identity not persisted before prompt: %+v %v", records, err)
	}
	stop()
	select {
	case err := <-done:
		if !errors.Is(err, harness.ErrStopUnconfirmed) {
			t.Fatalf("observer stop was not retained: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("observer did not leave")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := w.runner.executions.Shutdown(ctx); err != nil {
		t.Fatalf("detached observer did not join: %v", err)
	}
	return w, sessions, spec
}
func TestAuxiliaryRetainedPromptResumesWithoutSecondInputOrBudgetCharge(t *testing.T) {
	w, sessions, spec := retainedAuxFixture(t)
	before, _ := w.tasks.Get(w.work.ID)
	if before.Budget.Turns != 1 {
		t.Fatalf("initial budget=%+v", before.Budget)
	}
	next := New(sessions, w.runner.roster, w.runner.workspaces, w.attempts, execution.New(t.Context(), w.tasks), w.runner.budget)
	var validated string
	result, err := next.Prompt(t.Context(), spec, "verify original work", func(answer string) error { validated = answer; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != sessions.answer || validated != sessions.answer || result.Attempt.State != attempt.Bound || result.Usage.Input != 17 {
		t.Fatalf("retained result differs: %+v", result)
	}
	again, err := next.Prompt(t.Context(), spec, "verify original work", func(answer string) error {
		if answer != sessions.answer {
			t.Error("cached validator missed full answer")
		}
		return nil
	})
	if err != nil || again.Attempt.ID != result.Attempt.ID {
		t.Fatalf("cache=%+v %v", again, err)
	}
	after, _ := w.tasks.Get(w.work.ID)
	if after.Budget.Turns != 1 {
		t.Fatalf("resuming charged budget again: %+v", after.Budget)
	}
	sessions.mu.Lock()
	defer sessions.mu.Unlock()
	if sessions.prompted != 1 || sessions.resumed != 1 {
		t.Fatalf("input repeated: prompt=%d resume=%d", sessions.prompted, sessions.resumed)
	}
}
func TestAuxiliaryRetainedChangedInputIsBlockedWithoutReplay(t *testing.T) {
	w, sessions, spec := retainedAuxFixture(t)
	next := New(sessions, w.runner.roster, w.runner.workspaces, w.attempts, execution.New(t.Context(), w.tasks), w.runner.budget)
	_, err := next.Prompt(t.Context(), spec, "different verification", nil)
	if err == nil || !strings.Contains(err.Error(), "原") {
		t.Fatalf("changed work accepted: %v", err)
	}
	sessions.mu.Lock()
	defer sessions.mu.Unlock()
	if sessions.prompted != 1 || sessions.resumed != 0 {
		t.Fatal("changed input reached original execution")
	}
}
