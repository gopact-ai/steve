package turn

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/task"
)

// driveChild stands in for the delegation service driving a child on a
// detached scope: when the scope is stopped it records how the child
// ended and lets the scope go, the way completeChild does.
func driveChild(t *testing.T, tasks *task.Store, scope *execution.Scope, childID string) {
	t.Helper()
	go func() {
		<-scope.Context().Done()
		if err := tasks.SetResult(childID, task.Result{Outcome: task.OutcomeCancelled, Answer: "half done"}); err != nil {
			t.Error(err)
		}
		scope.Finish(nil)
	}()
}

// delegateChild opens a running child of task 1 with an execution the
// stop has to reach, and returns it.
func delegateChild(t *testing.T, coordinator *Coordinator, tasks *task.Store, name string) (task.Task, *execution.Scope) {
	t.Helper()
	child, err := tasks.Spawn("1", task.Task{Member: name, Node: "dev", Origin: "delegate:1", Goal: "part of it"})
	if err != nil {
		t.Fatal(err)
	}
	if child, err = tasks.Advance(child.ID, task.StateRunning); err != nil {
		t.Fatal(err)
	}
	scope, err := coordinator.executions.Begin(t.Context(), execution.Key{TaskID: child.ID, InstanceID: "delegate/" + name})
	if err != nil {
		t.Fatal(err)
	}
	driveChild(t, tasks, scope, child.ID)
	return child, scope
}

// rearm readies the fake for another turn that blocks until it is
// cancelled. Only between turns: the previous prompt has returned.
func (r *fakeRunner) rearm() {
	r.started, r.done = make(chan struct{}), make(chan struct{})
	r.start, r.stop = sync.Once{}, sync.Once{}
	r.canceled.Store(false)
}

type prefaceSpy struct {
	mu    sync.Mutex
	calls []string
	told  int
}

func (p *prefaceSpy) preface(_ context.Context, taskID string) Preface {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, taskID)
	return Preface{Text: "[steve: preface for #" + taskID + "]", Told: func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.told++
	}}
}

func (p *prefaceSpy) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.calls...)
}

func (p *prefaceSpy) toldTimes() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.told
}

// Stop is one gesture for everything the user's task set in motion: the
// turn, and the children it delegated. Nothing may then continue on its
// own — the next word is the user's, and that turn opens with what the
// children left. The stopped turn settles the way a harness settles a
// cancel, with ErrTurnCanceled: its own account is given, and that must
// not lift the hold the stop just placed.
func TestCancelStopsDelegatedChildrenAndHoldsTheTaskUntilTheNextTurn(t *testing.T) {
	runner := &fakeRunner{reply: "ok", started: make(chan struct{}), done: make(chan struct{}), cancelSettles: true}
	coordinator, tasks := taskCoordinator(t, runner)
	spy := &prefaceSpy{}
	coordinator.SetTurnPreface(spy.preface)

	first := make(chan error, 1)
	go func() {
		_, err := handle(coordinator, t.Context(), "a long job")
		first <- err
	}()
	<-runner.started
	child, scope := delegateChild(t, coordinator, tasks, "child")

	result, err := handle(coordinator, t.Context(), "/cancel")
	if err != nil {
		t.Fatalf("/cancel: %v", err)
	}
	zh := i18n.New(i18n.LocaleZH)
	if !strings.Contains(result.Text, zh.T(i18n.CancelRequested, "codex")) || !strings.Contains(result.Text, "#"+child.ID) || !strings.Contains(result.Text, "child@dev") {
		t.Fatalf("reply = %q; want the turn cancelled and the stopped child named", result.Text)
	}
	select {
	case err := <-first:
		if !errors.Is(err, harness.ErrTurnCanceled) {
			t.Fatalf("cancelled turn ended with %v; want ErrTurnCanceled", err)
		}
	case <-time.After(waitDeadline):
		t.Fatal("cancel did not stop the running turn")
	}
	if stopped, _ := tasks.Get(child.ID); stopped.State != task.StateCancelled || stopped.Result == nil {
		t.Fatalf("child = %s result=%+v; want cancelled with its result kept", stopped.State, stopped.Result)
	}
	if scope.Context().Err() == nil {
		t.Fatal("the child's execution was not stopped")
	}
	parent, _ := tasks.Get("1")
	if parent.State != task.StateRunning || !parent.Held() {
		t.Fatalf("parent = %s held=%v; want still running, and held", parent.State, parent.Held())
	}
	// Every turn opens with its task's preface; the first one did, and the
	// stop itself composes nothing. The cancelled turn settled, so its
	// preface counts as given.
	if calls := spy.seen(); len(calls) != 1 || calls[0] != "1" {
		t.Fatalf("preface calls after the stop = %v; want only the first turn's", calls)
	}
	if spy.toldTimes() != 1 {
		t.Fatalf("preface told %d time(s) after the settled cancel; want 1", spy.toldTimes())
	}

	// The next message continues the same task: the children's status is
	// put in front of it, and the hold is lifted.
	runner.rearm()
	close(runner.done)
	if _, err := handle(coordinator, t.Context(), "continue"); err != nil {
		t.Fatalf("follow-up turn: %v", err)
	}
	seen := runner.seen()
	last := seen[len(seen)-1]
	if before, _, ok := strings.Cut(last, "continue"); !ok || !strings.Contains(before, "[steve: preface for #1]") {
		t.Fatalf("prompt = %q; want the preface before the user's message", last)
	}
	if calls := spy.seen(); len(calls) != 2 || calls[1] != "1" {
		t.Fatalf("preface calls = %v; want the continued turn to ask for task 1", calls)
	}
	after, _ := tasks.Get("1")
	if after.Held() {
		t.Fatal("the hold survived the user's next turn")
	}
	if spy.toldTimes() != 2 {
		t.Fatalf("preface told %d time(s); want each settled turn to record its account as given", spy.toldTimes())
	}
	if all := tasks.List("chat"); len(all) != 2 {
		t.Fatalf("tasks = %d; want the same task continued, not a new one", len(all))
	}
}

// A stop during the continued turn is a stop like the first: the turn it
// cancels was composed under the earlier stop and, settling, gives that
// account — but the hold it finds is the later stop's, and stays.
func TestASecondStopDuringTheContinuedTurnKeepsTheTaskHeld(t *testing.T) {
	runner := &fakeRunner{reply: "ok", started: make(chan struct{}), done: make(chan struct{}), cancelSettles: true}
	coordinator, tasks := taskCoordinator(t, runner)
	spy := &prefaceSpy{}
	coordinator.SetTurnPreface(spy.preface)

	first := make(chan error, 1)
	go func() {
		_, err := handle(coordinator, t.Context(), "a long job")
		first <- err
	}()
	<-runner.started
	delegateChild(t, coordinator, tasks, "one")
	if _, err := handle(coordinator, t.Context(), "/cancel"); err != nil {
		t.Fatalf("first /cancel: %v", err)
	}
	<-first

	runner.rearm()
	second := make(chan error, 1)
	go func() {
		_, err := handle(coordinator, t.Context(), "continue")
		second <- err
	}()
	<-runner.started
	// The agent, continuing, delegates again; the user stops again.
	again, scope := delegateChild(t, coordinator, tasks, "two")
	if _, err := handle(coordinator, t.Context(), "/cancel"); err != nil {
		t.Fatalf("second /cancel: %v", err)
	}
	select {
	case err := <-second:
		if !errors.Is(err, harness.ErrTurnCanceled) {
			t.Fatalf("continued turn ended with %v; want ErrTurnCanceled", err)
		}
	case <-time.After(waitDeadline):
		t.Fatal("the second cancel did not stop the continued turn")
	}
	if stopped, _ := tasks.Get(again.ID); stopped.State != task.StateCancelled || scope.Context().Err() == nil {
		t.Fatalf("second child = %s stopped=%v; want cancelled and its execution stopped", stopped.State, scope.Context().Err() != nil)
	}
	if parent, _ := tasks.Get("1"); !parent.Held() {
		t.Fatal("the continued turn's settled account lifted the second stop's hold")
	}
	if spy.toldTimes() != 2 {
		t.Fatalf("preface told %d time(s); want both settled turns to record their account", spy.toldTimes())
	}
}

// A preface counts as given only when the prompt settled with the agent.
// A turn that fails before the harness answers keeps the hold and leaves
// the account to the next turn, like a delivery without its receipt.
func TestPrefaceIsToldAgainWhenTheTurnNeverSettled(t *testing.T) {
	runner := &fakeRunner{err: errors.New("session lost before the prompt")}
	coordinator, tasks := taskCoordinator(t, runner)
	spy := &prefaceSpy{}
	coordinator.SetTurnPreface(spy.preface)
	if _, err := handle(coordinator, t.Context(), "start it"); err == nil {
		t.Fatal("the failing runner let the turn succeed")
	}
	if _, err := tasks.Hold("1"); err != nil {
		t.Fatal(err)
	}
	if _, err := handle(coordinator, t.Context(), "continue"); err == nil {
		t.Fatal("the failing runner let the turn succeed")
	}
	if held, _ := tasks.Get("1"); !held.Held() || spy.toldTimes() != 0 {
		t.Fatalf("held=%v told=%d after a turn the agent never answered; want still held and untold", held.Held(), spy.toldTimes())
	}
	runner.err = nil
	if _, err := handle(coordinator, t.Context(), "continue"); err != nil {
		t.Fatal(err)
	}
	if released, _ := tasks.Get("1"); released.Held() || spy.toldTimes() != 1 {
		t.Fatalf("held=%v told=%d after a settled turn; want released and told once", released.Held(), spy.toldTimes())
	}
	seen := runner.seen()
	if last := seen[len(seen)-1]; !strings.Contains(last, "[steve: preface for #1]") {
		t.Fatalf("the settled turn's prompt lacks the preface: %q", last)
	}
}

// Children outlive the turn that delegated them by design, so Stop has to
// reach them even when no turn is running. The reply invites the next
// message, so that message must not be swallowed by the window a stop
// arms against a turn that was just starting.
func TestCancelStopsDelegatedChildrenWhenNoTurnIsRunning(t *testing.T) {
	runner := &fakeRunner{reply: "ok"}
	coordinator, tasks := taskCoordinator(t, runner)
	if _, err := handle(coordinator, t.Context(), "start it"); err != nil {
		t.Fatal(err)
	}
	child, _ := delegateChild(t, coordinator, tasks, "child")

	result, err := handle(coordinator, t.Context(), "/cancel")
	if err != nil {
		t.Fatalf("/cancel: %v", err)
	}
	zh := i18n.New(i18n.LocaleZH)
	if strings.Contains(result.Text, zh.T(i18n.NoRunningTurn)) || !strings.Contains(result.Text, "#"+child.ID) {
		t.Fatalf("reply = %q; want the stopped child named, not 'nothing running'", result.Text)
	}
	if stopped, _ := tasks.Get(child.ID); stopped.State != task.StateCancelled {
		t.Fatalf("child = %s; want cancelled", stopped.State)
	}
	if parent, _ := tasks.Get("1"); !parent.Held() || parent.State != task.StateRunning {
		t.Fatalf("parent held=%v state=%s", parent.Held(), parent.State)
	}
	if _, err := handle(coordinator, t.Context(), "continue"); err != nil {
		t.Fatalf("the message right after the stop was not run: %v", err)
	}
	if seen := runner.seen(); !strings.Contains(seen[len(seen)-1], "continue") {
		t.Fatalf("prompts = %q; want the continuation to reach the agent", seen)
	}
}

// The agent may still delegate while its turn is being stopped; the stop
// reaches what it finds once the turn has ended, not what it saw first.
func TestCancelStopsAChildDelegatedWhileTheTurnWasStopping(t *testing.T) {
	runner := &fakeRunner{reply: "ok", started: make(chan struct{}), done: make(chan struct{}), cancelSettles: true}
	coordinator, tasks := taskCoordinator(t, runner)
	var late task.Task
	var lateScope *execution.Scope
	runner.onCancel = func() { late, lateScope = delegateChild(t, coordinator, tasks, "late") }
	first := make(chan error, 1)
	go func() {
		_, err := handle(coordinator, t.Context(), "a long job")
		first <- err
	}()
	<-runner.started
	result, err := handle(coordinator, t.Context(), "/cancel")
	if err != nil {
		t.Fatalf("/cancel: %v", err)
	}
	<-first
	if stopped, _ := tasks.Get(late.ID); stopped.State != task.StateCancelled || lateScope.Context().Err() == nil {
		t.Fatalf("late child = %s stopped=%v; want cancelled and its execution stopped", stopped.State, lateScope.Context().Err() != nil)
	}
	if !strings.Contains(result.Text, "#"+late.ID) {
		t.Fatalf("reply = %q; want the late child named", result.Text)
	}
	if parent, _ := tasks.Get("1"); !parent.Held() {
		t.Fatal("a task whose child was stopped is not held")
	}
}

// A plain cancel with nothing delegated is what it always was.
func TestCancelWithoutChildrenLeavesTheTaskUnheld(t *testing.T) {
	runner := &fakeRunner{reply: "ok", started: make(chan struct{}), done: make(chan struct{})}
	coordinator, tasks := taskCoordinator(t, runner)
	first := make(chan error, 1)
	go func() {
		_, err := handle(coordinator, t.Context(), "a long job")
		first <- err
	}()
	<-runner.started
	result, err := handle(coordinator, t.Context(), "/cancel")
	if err != nil || result.Text != i18n.New(i18n.LocaleZH).T(i18n.CancelRequested, "codex") {
		t.Fatalf("cancel = %#v, %v", result, err)
	}
	<-first
	if parent, _ := tasks.Get("1"); parent.Held() {
		t.Fatal("a task with nothing delegated was held")
	}
}
