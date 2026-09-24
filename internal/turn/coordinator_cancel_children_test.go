package turn

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/view"
)

// drivenChild is a delegated child of task 1 with an execution the stop
// has to reach, driven the way the delegation service drives one on a
// detached scope: when the scope is stopped it records how the child
// ended and lets the scope go, the way completeChild does. parentHeld is
// what the parent looked like at that moment — the moment completeChild
// would deliver the result, so the parent must be held then.
type drivenChild struct {
	task.Task
	scope      *execution.Scope
	ended      chan struct{}
	parentHeld bool
}

func delegateChild(t *testing.T, coordinator *Coordinator, tasks *task.Store, name string) *drivenChild {
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
	d := &drivenChild{Task: child, scope: scope, ended: make(chan struct{})}
	go func() {
		defer close(d.ended)
		<-scope.Context().Done()
		parent, _ := tasks.Get("1")
		d.parentHeld = parent.Held()
		if err := tasks.SetResult(child.ID, task.Result{Outcome: task.OutcomeCancelled, Answer: "half done"}); err != nil {
			t.Error(err)
		}
		scope.Finish(nil)
	}()
	return d
}

// stopped waits for the child to have ended and checks it ended the way
// a stop ends a child: cancelled, its execution stopped, its parent held.
func (d *drivenChild) stopped(t *testing.T, tasks *task.Store) {
	t.Helper()
	select {
	case <-d.ended:
	case <-time.After(waitDeadline):
		t.Fatalf("child #%s never ended", d.ID)
	}
	if got, _ := tasks.Get(d.ID); got.State != task.StateCancelled || got.Result == nil {
		t.Fatalf("child #%s = %s result=%+v; want cancelled with its result kept", d.ID, got.State, got.Result)
	}
	if d.scope.Context().Err() == nil {
		t.Fatalf("child #%s's execution was not stopped", d.ID)
	}
	if !d.parentHeld {
		t.Fatalf("child #%s ended while its parent was not held: its result would have woken the task", d.ID)
	}
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
	spy := &prefaceSpy{}
	coordinator, tasks := taskCoordinator(t, runner, withCallbacks(func(cb *Callbacks) { cb.TurnPreface = spy.preface }))

	first := make(chan error, 1)
	go func() {
		_, err := handle(coordinator, t.Context(), "a long job")
		first <- err
	}()
	<-runner.started
	child := delegateChild(t, coordinator, tasks, "child")

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
	child.stopped(t, tasks)
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
	spy := &prefaceSpy{}
	coordinator, tasks := taskCoordinator(t, runner, withCallbacks(func(cb *Callbacks) { cb.TurnPreface = spy.preface }))

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
	again := delegateChild(t, coordinator, tasks, "two")
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
	again.stopped(t, tasks)
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
	spy := &prefaceSpy{}
	coordinator, tasks := taskCoordinator(t, runner, withCallbacks(func(cb *Callbacks) { cb.TurnPreface = spy.preface }))
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
	child := delegateChild(t, coordinator, tasks, "child")

	result, err := handle(coordinator, t.Context(), "/cancel")
	if err != nil {
		t.Fatalf("/cancel: %v", err)
	}
	zh := i18n.New(i18n.LocaleZH)
	if strings.Contains(result.Text, zh.T(i18n.NoRunningTurn)) || !strings.Contains(result.Text, "#"+child.ID) {
		t.Fatalf("reply = %q; want the stopped child named, not 'nothing running'", result.Text)
	}
	child.stopped(t, tasks)
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
	var late *drivenChild
	runner.onCancel = func() { late = delegateChild(t, coordinator, tasks, "late") }
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
	late.stopped(t, tasks)
	if !strings.Contains(result.Text, "#"+late.ID) {
		t.Fatalf("reply = %q; want the late child named", result.Text)
	}
	if parent, _ := tasks.Get("1"); !parent.Held() {
		t.Fatal("a task whose child was stopped is not held")
	}
}

// A child the agent delegates while its turn is being stopped may end on
// its own before the stop reaches it. The task is held from the moment
// the stop begins, so the cancelled turn's own delivery pass finds it
// held, and the child's result waits for the user's next message.
func TestAChildEndingOnItsOwnDuringTheStopFindsTheTaskHeld(t *testing.T) {
	runner := &fakeRunner{reply: "ok", started: make(chan struct{}), done: make(chan struct{}), cancelSettles: true}
	afterTurn := func(string) {}
	coordinator, tasks := taskCoordinator(t, runner, withCallbacks(func(cb *Callbacks) {
		cb.AfterTurn = func(taskID string) { afterTurn(taskID) }
	}))
	runner.onCancel = func() {
		quick, err := tasks.Spawn("1", task.Task{Member: "quick", Node: "dev", Origin: "delegate:1", Goal: "a small part"})
		if err != nil {
			t.Error(err)
			return
		}
		for _, to := range []task.State{task.StateRunning, task.StateDone} {
			if _, err := tasks.Advance(quick.ID, to); err != nil {
				t.Error(err)
				return
			}
		}
		if err := tasks.SetResult(quick.ID, task.Result{Outcome: task.OutcomeOK, Answer: "done already"}); err != nil {
			t.Error(err)
		}
	}
	heldAtEnd := make(chan bool, 1)
	afterTurn = func(taskID string) {
		parent, _ := tasks.Get(taskID)
		heldAtEnd <- parent.Held()
	}
	first := make(chan error, 1)
	go func() {
		_, err := handle(coordinator, t.Context(), "a long job")
		first <- err
	}()
	<-runner.started
	if _, err := handle(coordinator, t.Context(), "/cancel"); err != nil {
		t.Fatalf("/cancel: %v", err)
	}
	<-first
	if held := <-heldAtEnd; !held {
		t.Fatal("the cancelled turn's delivery pass found the task unheld: the child's result would have woken it")
	}
	if parent, _ := tasks.Get("1"); !parent.Held() {
		t.Fatal("a task with a child's result to account for is not held after the stop")
	}
}

// composingRunner is a session whose settings are read as the turn is
// armed: once the session is reachable for /cancel, before the prompt is
// composed. One read can be made to wait, which is where a stop lands
// between the two.
type composingRunner struct {
	*fakeRunner
	mu   sync.Mutex
	wait func()
}

// onNextSettingsRead makes the next read of the settings run fn first.
func (r *composingRunner) onNextSettingsRead(fn func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.wait = fn
}

func (r *composingRunner) Settings() view.Settings {
	r.mu.Lock()
	wait := r.wait
	r.wait = nil
	r.mu.Unlock()
	if wait != nil {
		wait()
	}
	return view.Settings{}
}
func (r *composingRunner) ModelChoices() (string, []view.Choice)           { return "", nil }
func (r *composingRunner) SetModel(context.Context, string, string) error  { return nil }
func (r *composingRunner) SetOption(context.Context, string, string) error { return nil }

// composingRuntime opens the composingRunner as the codex session.
type composingRuntime struct {
	*fakeManager
	runner *composingRunner
}

func (m *composingRuntime) OpenSession(ctx context.Context, at harness.Placement, upstream, workdir string, servers []acp.MCPServer) (harness.Runner, error) {
	if _, err := m.fakeManager.OpenSession(ctx, at, upstream, workdir, servers); err != nil {
		return nil, err
	}
	return m.runner, nil
}

// A stop can land while a turn is armed but not yet composed. That turn
// then composes under the stop's own stamp and, cancelled, settles with
// its account given — but it is the turn the stop cancelled, and its
// end must not lift the stop's hold: the children are stopped next, and
// what they leave waits for the user.
func TestATurnStoppedWhileComposingDoesNotLiftTheStopsHold(t *testing.T) {
	runner := &composingRunner{fakeRunner: &fakeRunner{reply: "ok", cancelSettles: true}}
	rt := &composingRuntime{fakeManager: &fakeManager{runners: map[string]*fakeRunner{"codex": runner.fakeRunner}}, runner: runner}
	afterTurn := func(string) {}
	coordinator, tasks, _ := taskCoordinatorOn(t, rt, withCallbacks(func(cb *Callbacks) {
		cb.AfterTurn = func(taskID string) { afterTurn(taskID) }
	}))
	if _, err := handle(coordinator, t.Context(), "start it"); err != nil {
		t.Fatal(err)
	}
	child := delegateChild(t, coordinator, tasks, "child")
	composing, gate := make(chan struct{}), make(chan struct{})
	runner.onNextSettingsRead(func() {
		close(composing)
		<-gate
	})
	heldAtEnd := make(chan bool, 1)
	afterTurn = func(taskID string) {
		parent, _ := tasks.Get(taskID)
		heldAtEnd <- parent.Held()
	}
	cancelled := make(chan struct{})
	runner.onCancel = func() { close(cancelled) }
	first := make(chan error, 1)
	go func() {
		_, err := handle(coordinator, t.Context(), "a long job")
		first <- err
	}()
	<-composing
	stop := make(chan error, 1)
	go func() {
		_, err := handle(coordinator, t.Context(), "/cancel")
		stop <- err
	}()
	<-cancelled
	close(gate)
	if err := <-first; !errors.Is(err, harness.ErrTurnCanceled) {
		t.Fatalf("stopped turn ended with %v; want ErrTurnCanceled", err)
	}
	if err := <-stop; err != nil {
		t.Fatalf("/cancel: %v", err)
	}
	if held := <-heldAtEnd; !held {
		t.Fatal("the turn the stop cancelled lifted the hold when it ended")
	}
	child.stopped(t, tasks)
	if parent, _ := tasks.Get("1"); !parent.Held() {
		t.Fatal("the task is not held after the stop")
	}
}

// A preface lifts only the hold it was composed under, and only when the
// turn was not the one a stop cancelled: a later stop re-stamps the hold,
// and neither the earlier turn's settling nor the stopped turn's lifts it.
func TestAPrefaceLiftsOnlyTheHoldItWasComposedUnder(t *testing.T) {
	// The spy takes over after the first turn, which runs without one.
	preface := func(context.Context, string) Preface { return Preface{} }
	coordinator, tasks := taskCoordinator(t, &fakeRunner{reply: "ok"}, withCallbacks(func(cb *Callbacks) {
		cb.TurnPreface = func(ctx context.Context, taskID string) Preface { return preface(ctx, taskID) }
	}))
	if _, err := handle(coordinator, t.Context(), "start it"); err != nil {
		t.Fatal(err)
	}
	spy := &prefaceSpy{}
	preface = spy.preface
	if _, err := tasks.Hold("1"); err != nil {
		t.Fatal(err)
	}
	_, told := coordinator.preface(t.Context(), "1")
	if _, err := tasks.Hold("1"); err != nil {
		t.Fatal(err)
	}
	told(true)
	if parent, _ := tasks.Get("1"); !parent.Held() {
		t.Fatal("a turn composed under the earlier stop lifted the later one's hold")
	}
	_, told = coordinator.preface(t.Context(), "1")
	told(false)
	if parent, _ := tasks.Get("1"); !parent.Held() {
		t.Fatal("the turn a stop cancelled lifted its hold")
	}
	_, told = coordinator.preface(t.Context(), "1")
	told(true)
	if parent, _ := tasks.Get("1"); parent.Held() {
		t.Fatal("a turn composed under the current stop did not lift its hold")
	}
	if spy.toldTimes() != 3 {
		t.Fatalf("preface told %d time(s); want every settled turn's account recorded, lifted or not", spy.toldTimes())
	}
}

// A turn that could not be stopped as asked is still reported first: the
// reply leads with why, as it always did, and the children stopped after
// it are named below. The error alone does not reach the transcript.
func TestCancelReplyLeadsWithTheTurnsErrorBeforeTheStoppedChildren(t *testing.T) {
	refused := errors.New("agent refused the stop")
	runner := &fakeRunner{reply: "ok", started: make(chan struct{}), done: make(chan struct{}), cancelErr: refused}
	coordinator, tasks := taskCoordinator(t, runner)
	first := make(chan error, 1)
	go func() {
		_, err := handle(coordinator, t.Context(), "a long job")
		first <- err
	}()
	<-runner.started
	child := delegateChild(t, coordinator, tasks, "child")
	result, err := handle(coordinator, t.Context(), "/cancel")
	if !errors.Is(err, refused) {
		t.Fatalf("/cancel: %v; want the agent's refusal", err)
	}
	lines := strings.Split(result.Text, "\n")
	if lines[0] != refused.Error() {
		t.Fatalf("reply = %q; want it to lead with why the turn was not stopped", result.Text)
	}
	if !strings.Contains(result.Text, "#"+child.ID) {
		t.Fatalf("reply = %q; want the stopped child named", result.Text)
	}
	<-first
	child.stopped(t, tasks)
}

// A child whose stop was never confirmed — cancelled, with no result — is
// reported while the stop that cancelled it holds the task, then left to
// the recovery flow. It does not make every later stop hold the task
// again: with nothing else delegated, that stop is what it always was.
func TestAChildStoppedUnconfirmedDoesNotHoldEveryLaterStop(t *testing.T) {
	runner := &fakeRunner{reply: "ok", started: make(chan struct{}), done: make(chan struct{})}
	coordinator, tasks := taskCoordinator(t, runner)
	first := make(chan error, 1)
	go func() {
		_, err := handle(coordinator, t.Context(), "a long job")
		first <- err
	}()
	<-runner.started
	child, err := tasks.Spawn("1", task.Task{Member: "child", Node: "dev", Origin: "delegate:1", Goal: "part of it"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Advance(child.ID, task.StateRunning); err != nil {
		t.Fatal(err)
	}
	// Stopped earlier, and its execution never confirmed: no result.
	if _, err := tasks.SetAside(child.ID, task.StateCancelled); err != nil {
		t.Fatal(err)
	}
	result, err := handle(coordinator, t.Context(), "/cancel")
	if err != nil || result.Text != i18n.New(i18n.LocaleZH).T(i18n.CancelRequested, "codex") {
		t.Fatalf("cancel = %#v, %v", result, err)
	}
	<-first
	if parent, _ := tasks.Get("1"); parent.Held() {
		t.Fatal("a child whose earlier stop was never confirmed held the task at a later stop")
	}
}

// A stop's hold lasts until a turn gives its account. A second stop
// before the user's next message — nothing running, nothing left to
// stop — finds the task held for the child the first stop cancelled
// without confirmation, and leaves it so: that child is still owed to
// the next turn, and only that turn may lift the hold.
func TestASecondStopBeforeTheNextMessageKeepsTheTaskHeld(t *testing.T) {
	runner := &fakeRunner{reply: "ok", started: make(chan struct{}), done: make(chan struct{})}
	coordinator, tasks := taskCoordinator(t, runner)
	first := make(chan error, 1)
	go func() {
		_, err := handle(coordinator, t.Context(), "a long job")
		first <- err
	}()
	<-runner.started
	child, err := tasks.Spawn("1", task.Task{Member: "child", Node: "dev", Origin: "delegate:1", Goal: "part of it"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Advance(child.ID, task.StateRunning); err != nil {
		t.Fatal(err)
	}
	// What the first stop left: the child cancelled, its execution never
	// confirming, and the task held for it.
	if _, err := tasks.SetAside(child.ID, task.StateCancelled); err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Hold("1"); err != nil {
		t.Fatal(err)
	}
	if _, err := handle(coordinator, t.Context(), "/cancel"); err != nil {
		t.Fatalf("/cancel: %v", err)
	}
	<-first
	if parent, _ := tasks.Get("1"); !parent.Held() {
		t.Fatal("a second stop lifted the hold the first stop placed for a child whose stop was never confirmed")
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
