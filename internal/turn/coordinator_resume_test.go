package turn

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/view"
)

type retainedTestRunner struct {
	*fakeRunner
	state       nodewire.SessionState
	resumeCalls int
	ask         bool
}

func (r *retainedTestRunner) InspectRetained(context.Context) (nodewire.SessionState, error) {
	return r.state, nil
}
func (r *retainedTestRunner) ResumeTurn(ctx context.Context, _ permission.AskFunc, ask acphost.AskUserFunc, progress func(view.Progress)) (string, []string, error) {
	r.resumeCalls++
	if r.ask {
		answer, err := ask(ctx, view.Question{Message: "Continue the original command?", Choices: []view.Choice{{Value: "yes", Label: "Continue"}}, Required: true})
		if err != nil {
			return "", nil, err
		}
		if answer.Value != "yes" {
			return "", nil, errors.New("original question was not answered")
		}
	}
	progress(view.Progress{Usage: view.Usage{InputTokens: 7, OutputTokens: 11}})
	return r.reply, []string{"retained execution completed"}, nil
}

type retainedTestManager struct {
	*fakeManager
	runner *retainedTestRunner
}

func (m retainedTestManager) AttachRetainedSession(ctx context.Context, place harness.Placement, id, workdir string) (harness.ResumableRunner, error) {
	key, ok := execution.KeyOf(ctx)
	if !ok || key.AttemptID != m.runner.state.Binding.AttemptID {
		return nil, errors.New("missing original admitted execution scope")
	}
	return m.runner, nil
}

func retainedChatFixture(t *testing.T) (*Coordinator, *retainedTestRunner, *ledger.Ledger, attempt.Record, Request) {
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
	sessions, err := state.OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	cat, err := agent.NewCatalog(map[string]agent.Config{"worker": {Harness: "test", Node: "node-a", Default: true}})
	if err != nil {
		t.Fatal(err)
	}
	runner := &retainedTestRunner{fakeRunner: &fakeRunner{id: "ns_original", reply: strings.Repeat("complete result ", 30)}}
	manager := retainedTestManager{fakeManager: &fakeManager{}, runner: runner}
	c := New(cat, sessions, capability.NewAssembler(nil), manager, time.Minute)
	c.SetTasks(tasks, "coordinator-b")
	c.SetExecution(execution.New(t.Context(), tasks))
	c.SetAttempts(attempt.New(book))
	projects := project.Open(book)
	workspace := project.Workspace{ID: "workspace", Project: "p", Node: "node-a", Path: t.TempDir(), Kind: project.KindCanonical}
	if err := projects.Declare(t.Context(), []project.Project{{ID: "p", Home: project.Home{Node: "node-a", Path: workspace.Path}, Level: project.LevelInternal}}); err != nil {
		t.Fatal(err)
	}
	c.SetProjects(projects, "p", "")
	tracked, err := tasks.Create(task.Task{Goal: "original task", Channel: "console:main", Member: "worker", Requester: "owner", ProjectID: "p", Workspace: workspace.Path})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Begin(tracked.ID, "worker", "node-a", runner.id); err != nil {
		t.Fatal(err)
	}
	if err := tasks.SetAnchor(tracked.ID, "console", "web-e1", "p2p", ""); err != nil {
		t.Fatal(err)
	}
	token, err := tasks.ExecutionToken(tracked.ID)
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.attempts.Open(t.Context(), attempt.Spec{ID: "attempt-1", TaskID: tracked.ID, TurnID: "web-e1", Kind: attempt.KindChat, Project: "p", Node: "node-a", Harness: "test", Agent: "worker", Workspace: workspace, Scope: attempt.ScopeUnrestricted, Execution: &token})
	if err != nil {
		t.Fatal(err)
	}
	if err := tasks.BindAttempt(token, r.ID, r.TurnID); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []attempt.State{attempt.Prepared, attempt.Running} {
		r, err = c.attempts.Advance(t.Context(), r.ID, phase, "original", func(record *attempt.Record) { record.Session = runner.id })
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := sessions.SaveSession(state.Session{ConversationID: tracked.Channel, AgentID: "worker", HarnessID: "test", NodeID: "node-a", UpstreamID: runner.id, Workspace: workspace.Path, ProjectID: "p", CapabilityHash: "existing-context", Tainted: true}); err != nil {
		t.Fatal(err)
	}
	runner.state = nodewire.SessionState{ID: runner.id, Harness: "test", State: "running", InputAccepted: 1, Binding: nodewire.SessionBinding{ProjectID: r.Project, SessionID: attempt.RetainedSessionID(tracked.Channel, tracked.ID, r.Agent), TaskID: r.TaskID, AttemptID: r.ID, NodeID: r.Node, ExecutionEpoch: r.Leases[0].Epoch, TaskEpoch: token.Epoch}, Command: &nodewire.SessionCommand{ID: r.TurnID, InputSequence: 1, State: "running"}}
	if _, err := c.attempts.PrepareRecovery(t.Context(), "replacement coordinator"); err != nil {
		t.Fatal(err)
	}
	return c, runner, book, r, Request{Channel: "console", ConversationID: tracked.Channel, MessageID: r.TurnID, ChatID: "console", SenderOpenID: "owner"}
}

func TestRetainedChatCompletesOriginalAttemptWithoutPromptReplay(t *testing.T) {
	c, runner, book, old, req := retainedChatFixture(t)
	if _, err := book.DB().Exec(`UPDATE leases SET expires_at = ?`, time.Now().Add(-time.Hour).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	runner.ask = true
	req.OnAskUser = func(_ context.Context, q view.Question) (view.Answer, error) {
		if q.Message != "Continue the original command?" {
			t.Fatal("question context changed")
		}
		return view.Answer{Value: "yes"}, nil
	}
	result, err := c.ResumeRetainedChat(t.Context(), old.ID, req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != runner.reply || result.Attempt != old.ID || runner.resumeCalls != 1 || len(runner.seen()) != 0 {
		t.Fatalf("execution was replayed or response lost: %+v", result)
	}
	record, err := c.attempts.Get(t.Context(), old.ID)
	if err != nil {
		t.Fatal(err)
	}
	var saved Result
	if record.State != attempt.Bound || record.Result == nil || json.Unmarshal(record.Result.Output, &saved) != nil || saved.Text != runner.reply {
		t.Fatalf("completion lost full response: %+v", record)
	}
	tracked, _ := c.tasks.Get(old.TaskID)
	if tracked.Budget.Turns != 1 || len(tracked.Attempts) != 1 || tracked.Attempts[0].Open() {
		t.Fatalf("task was duplicated or not settled: %+v", tracked)
	}
	if c.store.Conversation(req.ConversationID).Sessions["worker"].Tainted {
		t.Fatal("completed native session stayed tainted")
	}
	second, err := c.ResumeRetainedChat(t.Context(), old.ID, req)
	if err != nil || second.Text != runner.reply || runner.resumeCalls != 1 {
		t.Fatalf("committed result reran native command: %+v %v", second, err)
	}
}

func TestRetainedChatWithUncertainNodeRemainsOriginalBlockedExecution(t *testing.T) {
	c, runner, _, old, req := retainedChatFixture(t)
	runner.state.State = "interrupted"
	runner.state.Command.State = "uncertain"
	_, err := c.ResumeRetainedChat(t.Context(), old.ID, req)
	var blocked *RecoveryBlocked
	if !errors.As(err, &blocked) || blocked.Question.Kind != "recovery" || !strings.Contains(blocked.Question.Message, "已尝试") {
		t.Fatalf("missing intelligible recovery request: %v", err)
	}
	if runner.resumeCalls != 0 || len(runner.seen()) != 0 {
		t.Fatal("uncertain node command was resumed or replayed")
	}
	r, _ := c.attempts.Get(t.Context(), old.ID)
	if r.State != attempt.Running || !r.Unsettled {
		t.Fatalf("unknown writer released or failed: %+v", r)
	}
	tracked, _ := c.tasks.Get(old.TaskID)
	if !tracked.Attempts[0].Open() || tracked.Budget.Turns != 1 {
		t.Fatal("blocked observation changed task execution")
	}
}

type stoppedRetainedRunner struct {
	*retainedTestRunner
	coordinator *Coordinator
	taskID      string
}

func (r *stoppedRetainedRunner) ResumeTurn(ctx context.Context, _ permission.AskFunc, _ acphost.AskUserFunc, _ func(view.Progress)) (string, []string, error) {
	ids, err := r.coordinator.tasks.SetAside(r.taskID, task.StatePaused)
	if err != nil {
		return "", nil, err
	}
	r.coordinator.executions.Stop(ids, task.ErrExecutionStopped)
	return "partial before confirmed stop", nil, harness.ErrTurnCanceled
}

type stoppedRetainedManager struct {
	*fakeManager
	runner harness.ResumableRunner
}

type unavailableRetainedManager struct{ *fakeManager }

func (m unavailableRetainedManager) AttachRetainedSession(context.Context, harness.Placement, string, string) (harness.ResumableRunner, error) {
	return nil, harness.ErrNodeSessionUnavailable
}

func TestUnavailableRetainedNodeDoesNotBlockObserverGenerationShutdown(t *testing.T) {
	c, _, _, old, req := retainedChatFixture(t)
	lifetime, cancel := context.WithCancel(t.Context())
	defer cancel()
	c.SetExecution(execution.New(lifetime, c.tasks))
	c.runtime = unavailableRetainedManager{&fakeManager{}}
	if _, err := c.ResumeRetainedChat(lifetime, old.ID, req); err == nil {
		t.Fatal("unavailable node was reported resumed")
	}
	stop := c.executions.Stop([]string{old.TaskID}, task.ErrExecutionStopped)
	if err := stop.Wait(t.Context()); err == nil {
		t.Fatal("explicit stop fabricated remote stopping evidence")
	}
	cancel()
	if err := c.executions.Shutdown(t.Context()); err != nil {
		t.Fatalf("generation could not retire its unavailable-node observer: %v", err)
	}
	record, err := c.attempts.Get(t.Context(), old.ID)
	if err != nil || !record.Unsettled || record.State != attempt.Running {
		t.Fatalf("observer cleanup retired unknown remote execution: %+v %v", record, err)
	}
	tracked, _ := c.tasks.Get(old.TaskID)
	if !tracked.Attempts[0].Open() {
		t.Fatal("observer shutdown settled native task")
	}
}

func (m stoppedRetainedManager) AttachRetainedSession(context.Context, harness.Placement, string, string) (harness.ResumableRunner, error) {
	return m.runner, nil
}

func TestRetainedExplicitPauseSettlesAfterObserverCancellation(t *testing.T) {
	c, runner, _, old, req := retainedChatFixture(t)
	c.runtime = stoppedRetainedManager{fakeManager: &fakeManager{}, runner: &stoppedRetainedRunner{retainedTestRunner: runner, coordinator: c, taskID: old.TaskID}}
	_, err := c.ResumeRetainedChat(t.Context(), old.ID, req)
	if !errors.Is(err, harness.ErrTurnCanceled) {
		t.Fatalf("confirmed stop lost independent settlement: %v", err)
	}
	saved, err := c.attempts.Get(t.Context(), old.ID)
	if err != nil {
		t.Fatal(err)
	}
	tracked, _ := c.tasks.Get(old.TaskID)
	if saved.SessionSettled == nil || !*saved.SessionSettled || tracked.Attempts[len(tracked.Attempts)-1].Open() || tracked.State != task.StatePaused {
		t.Fatalf("confirmed pause not durably settled: attempt=%+v task=%+v", saved, tracked)
	}
}

func TestRetainedTaskAccountingFailureRemainsRetryableWithoutPromptReplay(t *testing.T) {
	c, runner, book, old, req := retainedChatFixture(t)
	if _, err := book.DB().Exec(`CREATE TRIGGER fail_accounting BEFORE INSERT ON bindings WHEN NEW.kind = 'document' AND NEW.id = 'tasks' BEGIN SELECT RAISE(ABORT, 'accounting unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	result, err := c.ResumeRetainedChat(t.Context(), old.ID, req)
	if err == nil || result.Attempt != "" || !strings.Contains(err.Error(), "accounting unavailable") {
		t.Fatalf("accounting failure swallowed: %+v %v", result, err)
	}
	if _, err := book.DB().Exec(`DROP TRIGGER fail_accounting`); err != nil {
		t.Fatal(err)
	}
	result, err = c.ResumeRetainedChat(t.Context(), old.ID, req)
	if err != nil || result.Attempt != old.ID || runner.resumeCalls != 1 {
		t.Fatalf("settlement retry replayed input or lost result: %+v %v calls=%d", result, err, runner.resumeCalls)
	}
}
