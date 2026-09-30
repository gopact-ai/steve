package console

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agentexec"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn"
	"github.com/gopact-ai/steve/internal/view"
)

// awaitStopWait waits for the card that says the stop of the original
// execution is being confirmed and offers to check again. A recovery that
// offers to retry the task in the meantime has gone back to asking about a
// task nothing may resume, and fails at once.
func awaitStopWait(t *testing.T, s *Service) consoleapi.PendingQuestion {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		for _, q := range s.Questions("main") {
			if q.State != "pending" {
				continue
			}
			for _, option := range q.Options {
				switch option.ID {
				case "retry":
					t.Fatalf("the recovery of a task set aside offered to retry it: %+v", q)
				case "recheck":
					return q
				}
			}
		}
	}
	t.Fatalf("no card waits on the stop: questions %+v, queue %+v", s.Questions("main"), s.Queue("main"))
	return consoleapi.PendingQuestion{}
}

func awaitStops(t *testing.T, stops *atomic.Int32) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); stops.Load() == 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the stop of the original execution was not attempted")
		}
	}
}

func queuedByID(s *Service, id string) Exchange {
	for _, e := range s.Queue("main") {
		if e.ID == id {
			return e
		}
	}
	return Exchange{}
}

// release finishes a held turn unless the test already did.
func release(call *queueCall) {
	select {
	case call.finish <- nil:
	default:
	}
}

func answerRecheck(t *testing.T, s *Service, card consoleapi.PendingQuestion) {
	t.Helper()
	if _, err := s.AnswerQuestion(t.Context(), card.ID, consoleapi.QuestionAnswer{CommandID: "recheck-" + card.ID, Decision: "accept", Choice: "recheck"}); err != nil {
		t.Fatal(err)
	}
}

// A task set aside while the console was down is known to the task store
// alone: nothing is left to tell the console. The restarted recovery goes
// by what the store holds, so it never offers to retry the task and waits
// on the stop of its original execution instead, saying whether the task
// was cancelled or paused. Confirming the stop ends the exchange and leaves
// the task as it was set aside.
func TestRecoveryAbandonsOnDurableStateAfterRestart(t *testing.T) {
	zh := i18n.New(i18n.LocaleZH)
	for _, tc := range []struct {
		state     task.State
		says, not i18n.Key
	}{
		{task.StateCancelled, i18n.ConsoleStopRecorded, i18n.ConsolePauseRecorded},
		{task.StatePaused, i18n.ConsolePauseRecorded, i18n.ConsoleStopRecorded},
	} {
		t.Run(string(tc.state), func(t *testing.T) {
			s := impatient(New(&echo{}, "owner", nil))
			s.EnableRetainedRecovery(t.Context())
			if err := s.Persist(recoveryDocument()); err != nil {
				t.Fatal(err)
			}
			driver := newCancelledTaskDriver()
			driver.setAside(tc.state, "task-1")
			if err := s.RecoverChats(t.Context(), driver); err != nil {
				t.Fatal(err)
			}
			card := awaitStopWait(t, s)
			if !strings.Contains(card.Message, zh.T(tc.says)) || strings.Contains(card.Message, zh.T(tc.not)) {
				t.Fatalf("the stop wait of a %s task says %q, want %q", tc.state, card.Message, zh.T(tc.says))
			}
			awaitStops(t, &driver.stops)
			if driver.calls.Load() != 0 {
				t.Fatalf("a %s task was resumed %d times", tc.state, driver.calls.Load())
			}
			if e := queuedByID(s, "e1"); e.State != consoleapi.ExchangeAwaitingUser {
				t.Fatalf("exchange while the stop is unconfirmed = %+v, want awaiting user", e)
			}
			driver.confirmed.Store(true)
			answerRecheck(t, s, card)
			if got := awaitExchange(t, s, "e1"); got.State != consoleapi.ExchangeCancelled {
				t.Fatalf("a confirmed stop did not end the exchange: %+v", got)
			}
			if got := driver.state("task-1"); got != tc.state {
				t.Fatalf("task after the stop confirmed = %s, want %s", got, tc.state)
			}
			awaitExchange(t, s, "e2")
		})
	}
}

// A task can be cancelled without the console hearing of it: stopping a
// turn cancels the delegated children at work under it in the task store
// alone. The next pass of a recovery bound to such a task finds it
// cancelled and stops offering to retry it.
func TestRecoveryAbandonsAfterCancelCascade(t *testing.T) {
	s := impatient(New(&echo{}, "owner", nil))
	s.EnableRetainedRecovery(t.Context())
	if err := s.Persist(recoveryDocument()); err != nil {
		t.Fatal(err)
	}
	driver := newCancelledTaskDriver()
	if err := s.RecoverChats(t.Context(), driver); err != nil {
		t.Fatal(err)
	}
	asked := awaitOffer(t, s, "retry")
	driver.setAside(task.StateCancelled, "task-1")
	if _, err := s.AnswerQuestion(t.Context(), asked.ID, consoleapi.QuestionAnswer{CommandID: "retry-1", Decision: "accept", Choice: "retry"}); err != nil {
		t.Fatal(err)
	}
	awaitStopWait(t, s)
	awaitStops(t, &driver.stops)
	if driver.calls.Load() != 1 {
		t.Fatalf("a cancelled task was resumed again: %d resumptions", driver.calls.Load())
	}
	if e := queuedByID(s, "e1"); e.State != consoleapi.ExchangeAwaitingUser {
		t.Fatalf("exchange while the stop is unconfirmed = %+v, want awaiting user", e)
	}
}

// stoppablePlanDriver holds the plan an exchange opened; the plan cannot
// be resumed, and its stop is never confirmed.
type stoppablePlanDriver struct {
	*planRecoveryDriver
	resumes atomic.Int32
	stops   atomic.Int32
}

func (d *stoppablePlanDriver) StopRetainedTask(_ context.Context, id string, req turn.Request, cancel bool) (turn.Result, error) {
	d.stops.Add(1)
	if id != "plan-task" || req.ConversationID != "console:main" || req.MessageID != "web-e1" {
		return turn.Result{}, errors.New("stop targeted another execution")
	}
	return turn.Result{}, harness.ErrStopUnconfirmed
}

// Cancelling a plan cancels its task in the task store alone as well. A
// recovery of the plan finds its task cancelled and waits on the stop
// instead of resuming the plan or offering to.
func TestRecoveryAbandonsAfterPlanCancel(t *testing.T) {
	s := impatient(New(&echo{}, "owner", nil))
	s.EnableRetainedRecovery(t.Context())
	if err := s.Persist(recoveryDocument()); err != nil {
		t.Fatal(err)
	}
	driver := &stoppablePlanDriver{planRecoveryDriver: &planRecoveryDriver{
		recoveryDriver: recoveryDriver{candidates: []turn.RetainedChat{}},
		plans:          []turn.RetainedPlan{{TaskID: "plan-task", Conversation: "console:main", MessageID: "web-e1", PlanID: "plan-1", RunID: "run-1", AttemptID: "planning-attempt", TaskState: task.StateCancelled}},
	}}
	driver.resumePlan = func(context.Context, turn.RetainedPlan, turn.Request) (turn.Result, error) {
		driver.resumes.Add(1)
		return turn.Result{}, &agentexec.RecoveryBlocked{Question: view.Question{Kind: "recovery", Message: "The plan's authorization changed.", Choices: []view.Choice{{Value: "retry", Label: "Retry"}}}}
	}
	if err := s.RecoverChats(t.Context(), driver); err != nil {
		t.Fatal(err)
	}
	awaitStopWait(t, s)
	awaitStops(t, &driver.stops)
	if driver.resumes.Load() != 0 {
		t.Fatalf("a cancelled plan was resumed %d times", driver.resumes.Load())
	}
	if e := queuedByID(s, "e1"); e.State != consoleapi.ExchangeAwaitingUser {
		t.Fatalf("exchange while the stop is unconfirmed = %+v, want awaiting user", e)
	}
}

// Choosing to stop in a recovery question sets the task aside. Until the
// original execution confirms the stop, the exchange waits on it on the
// card that offers to check again, as it does when the task is cancelled
// from the task list, instead of going back to the question.
func TestRecoveryStopChoiceUnconfirmedShowsRecheckCard(t *testing.T) {
	doc := recoveryDocument()
	s := impatient(New(&echo{}, "owner", nil))
	s.EnableRetainedRecovery(t.Context())
	if err := s.Persist(doc); err != nil {
		t.Fatal(err)
	}
	driver := newCancelledTaskDriver()
	if err := s.RecoverChats(t.Context(), driver); err != nil {
		t.Fatal(err)
	}
	asked := awaitOffer(t, s, "retry")
	if _, err := s.AnswerQuestion(t.Context(), asked.ID, consoleapi.QuestionAnswer{CommandID: "stop-1", Decision: "accept", Choice: "stop"}); err != nil {
		t.Fatal(err)
	}
	card := awaitStopWait(t, s)
	awaitStops(t, &driver.stops)
	if driver.calls.Load() != 1 {
		t.Fatalf("a task the owner stopped was resumed again: %d resumptions", driver.calls.Load())
	}
	if got := s.Queue("main"); got[0].State != consoleapi.ExchangeAwaitingUser || got[1].State != consoleapi.ExchangeQueued {
		t.Fatalf("queue while the stop is unconfirmed = %+v", got)
	}
	if raw, _, _ := doc.Load(); !strings.Contains(string(raw), `"recovery_stop_pending"`) {
		t.Fatalf("the stop the owner chose was not recorded: %s", raw)
	}
	driver.confirmed.Store(true)
	answerRecheck(t, s, card)
	if got := awaitExchange(t, s, "e1"); got.State != consoleapi.ExchangeCancelled {
		t.Fatalf("a confirmed stop did not end the exchange: %+v", got)
	}
	awaitExchange(t, s, "e2")
}

// storedTaskDriver finds the original execution of exchange e1 under a task
// kept in a task store, with the state the store holds. The execution
// cannot be resumed, and its stop is confirmed only once confirmed says so.
// While hold is set, a stop does not come back until held is closed.
type storedTaskDriver struct {
	tasks     *task.Store
	id        string
	calls     atomic.Int32
	stops     atomic.Int32
	confirmed atomic.Bool
	hold      atomic.Bool
	held      chan struct{}
}

func (d *storedTaskDriver) CheckRetainedChats(context.Context) error { return nil }

func (d *storedTaskDriver) RetainedChatsFor(context.Context, string, string) ([]turn.RetainedChat, error) {
	tracked, ok := d.tasks.Get(d.id)
	if !ok {
		return nil, fmt.Errorf("task %s missing", d.id)
	}
	return []turn.RetainedChat{{AttemptID: "attempt-1", TaskID: d.id, Conversation: "console:main", MessageID: "web-e1", AgentID: "worker", NodeID: "node-a", TaskState: tracked.State}}, nil
}

func (d *storedTaskDriver) ResumeRetainedChat(context.Context, string, turn.Request) (turn.Result, error) {
	d.calls.Add(1)
	return turn.Result{}, &agentexec.RecoveryBlocked{Question: view.Question{Kind: "recovery", Message: "The original machine is away.", Choices: []view.Choice{{Value: "retry", Label: "Retry"}}}}
}

func (d *storedTaskDriver) StopRetainedTask(ctx context.Context, id string, req turn.Request, cancel bool) (turn.Result, error) {
	d.stops.Add(1)
	if id != d.id || req.ConversationID != "console:main" || req.MessageID != "web-e1" {
		return turn.Result{}, errors.New("stop targeted another execution")
	}
	if d.hold.Load() {
		select {
		case <-d.held:
		case <-ctx.Done():
			return turn.Result{}, ctx.Err()
		}
	}
	if !d.confirmed.Load() {
		return turn.Result{}, harness.ErrStopUnconfirmed
	}
	return turn.Result{Text: "original task stopped"}, nil
}

// openStopWaitConsole restarts into a console kept in a ledger together
// with its task store. Exchange e1 was interrupted with its task moved to
// state and "follow-up" queued behind it; with stopping, e1 was already
// waiting on the stop of its original execution. Every turn the console
// starts is held by h until the test finishes it.
func openStopWaitConsole(t *testing.T, state task.State, stopping bool) (*Service, *queueHandler, *storedTaskDriver) {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := task.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	tracked, err := tasks.Create(task.Task{Transport: "console", Channel: "console:main", Member: "worker", Requester: "owner", Goal: "original"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Begin(tracked.ID, "worker", "node-a", "session"); err != nil {
		t.Fatal(err)
	}
	if state != task.StateRunning {
		if _, err := tasks.SetAside(tracked.ID, state); err != nil {
			t.Fatal(err)
		}
	}
	original := DurableExchange{Exchange: Exchange{ID: "e1", Conversation: "console:main", Input: "original", Key: "client:original", State: consoleapi.ExchangeRunning}}
	if stopping {
		original.State, original.RecoveryStopPending, original.RecoveryStopTask = consoleapi.ExchangeAwaitingUser, stopRequested, tracked.ID
	}
	saved := DurableState{
		Replies:   map[string][]consoleapi.Reply{"console:main": {{ID: "sent-1", Conversation: "console:main", ExchangeID: "e1", Kind: "sent", Input: "original"}}},
		Exchanges: map[string][]DurableExchange{"console:main": {original, {Exchange: Exchange{ID: "e2", Conversation: "console:main", Input: "follow-up", State: consoleapi.ExchangeQueued}}}},
	}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error { return StoreStateTx(tx, saved) }); err != nil {
		t.Fatal(err)
	}
	h := &queueHandler{started: make(chan *queueCall, 4)}
	s := impatient(New(h, "owner", nil))
	lifetime, stop := context.WithCancel(t.Context())
	s.EnableRetainedRecovery(lifetime)
	if err := s.PersistLedger(book); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stop()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Error(err)
		}
		book.Close()
	})
	driver := &storedTaskDriver{tasks: tasks, id: tracked.ID, held: make(chan struct{})}
	if err := s.RecoverChats(lifetime, driver); err != nil {
		t.Fatal(err)
	}
	return s, h, driver
}

// An exchange whose original execution only has to confirm the stop of a
// task its owner set aside no longer holds the conversation: what was
// queued behind it runs after a restart, and so does what is queued next,
// such as starting over with /new, while the exchange keeps waiting on the
// stop on its card.
func TestStopWaitExchangeDoesNotHoldQueue(t *testing.T) {
	for _, state := range []task.State{task.StateCancelled, task.StatePaused} {
		t.Run(string(state), func(t *testing.T) {
			s, h, driver := openStopWaitConsole(t, state, true)
			card := awaitStopWait(t, s)
			if err := s.Drain(); err != nil {
				t.Fatal(err)
			}
			call := nextCall(t, h)
			defer release(call)
			if call.req.Input != "follow-up" {
				t.Fatalf("started %q, want the line queued behind the stop wait", call.req.Input)
			}
			release(call)
			awaitExchange(t, s, "e2")
			reset := enqueueForTest(t, s, "main", "/new")
			next := nextCall(t, h)
			defer release(next)
			if next.req.Input != "/new" {
				t.Fatalf("started %q, want /new", next.req.Input)
			}
			release(next)
			awaitExchange(t, s, reset.ID)
			if e := queuedByID(s, "e1"); e.State != consoleapi.ExchangeAwaitingUser {
				t.Fatalf("exchange passed in the queue = %+v, want still awaiting user", e)
			}
			if q := questionByID(s, card.ID); q.State != "pending" {
				t.Fatalf("the stop wait card was taken down: %+v", q)
			}
			if driver.calls.Load() != 0 {
				t.Fatalf("a task set aside was resumed %d times", driver.calls.Load())
			}
		})
	}
}

// The exchange stands aside the moment it starts waiting on the stop of a
// task set aside, not once a stop pass comes back: a node slow to answer
// the stop does not hold what is queued behind it in the meantime.
func TestLineMovesWhileTheStopIsTried(t *testing.T) {
	s, h, driver := openStopWaitConsole(t, task.StateRunning, false)
	awaitOffer(t, s, "retry")
	if err := s.Drain(); err != nil {
		t.Fatal(err)
	}
	noCall(t, h)
	driver.hold.Store(true)
	defer close(driver.held)
	if _, err := driver.tasks.SetAside(driver.id, task.StateCancelled); err != nil {
		t.Fatal(err)
	}
	s.TasksCancelled()
	call := nextCall(t, h)
	defer release(call)
	if call.req.Input != "follow-up" {
		t.Fatalf("started %q, want the line queued behind the stop wait", call.req.Input)
	}
	awaitStops(t, &driver.stops)
	awaitStopWait(t, s)
	release(call)
	awaitExchange(t, s, "e2")
}

// What still needs the owner holds the conversation as before: a recovery
// question they have to answer, and a stop still to be confirmed for a task
// nobody set aside. Nothing queued behind either runs, /new included.
func TestAwaitingQuestionStillHoldsQueue(t *testing.T) {
	for _, tc := range []struct {
		name     string
		stopping bool
		card     string
	}{
		{"recovery question", false, "retry"},
		{"stop of a running task", true, "recheck"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, h, _ := openStopWaitConsole(t, task.StateRunning, tc.stopping)
			awaitOffer(t, s, tc.card)
			if err := s.Drain(); err != nil {
				t.Fatal(err)
			}
			noCall(t, h)
			reset := enqueueForTest(t, s, "main", "/new")
			noCall(t, h)
			if e := queuedByID(s, reset.ID); e.State != consoleapi.ExchangeQueued {
				t.Fatalf("/new behind a question = %+v, want queued", e)
			}
		})
	}
}

// Stopping from the composer stops what runs now. With the queue gone on
// past an exchange that only waits on a stop, that is the line running,
// not the exchange whose stop was already asked for.
func TestStopControlStopsTheLineRunningPastAStopWait(t *testing.T) {
	s, h, driver := openStopWaitConsole(t, task.StateCancelled, true)
	awaitStopWait(t, s)
	if err := s.Drain(); err != nil {
		t.Fatal(err)
	}
	running := nextCall(t, h)
	defer release(running)
	stops := driver.stops.Load()
	control := enqueueForTest(t, s, "main", "/cancel")
	call := nextCall(t, h)
	defer release(call)
	if call.req.Input != "/cancel" {
		t.Fatalf("started %q, want the stop", call.req.Input)
	}
	release(call)
	awaitExchange(t, s, control.ID)
	s.mu.Lock()
	var target *recoveryStopTarget
	for _, e := range s.exchanges["console:main"] {
		if e.ID == control.ID {
			target = e.RecoveryStopTarget
		}
	}
	s.mu.Unlock()
	if target != nil || driver.stops.Load() != stops {
		t.Fatalf("the stop went to the exchange waiting on a stop: target %+v, stops %d then %d", target, stops, driver.stops.Load())
	}
	release(running)
	awaitExchange(t, s, "e2")
}
