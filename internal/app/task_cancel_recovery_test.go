package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/console"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/gateway"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn"
	"github.com/gopact-ai/steve/internal/turn/turntest"
)

const cancelledRecoveryConversation = "console:main"

type cancelledRecovery struct {
	coordinator *turn.Coordinator
	tasks       *task.Store
	sessions    *state.Store
	projects    *project.Store
	cons        *console.Service
	taskID      string
}

// openCancelledRecovery restarts into a console exchange whose original
// execution is retained on a node that is not connected, with the
// production channels and callbacks wired. Its recovery puts the block to
// the owner at once.
func openCancelledRecovery(t *testing.T) cancelledRecovery {
	t.Helper()
	return openRetainedRecovery(t, "", task.StateRunning)
}

// openRetainedRecovery is openCancelledRecovery for a turn opened by
// origin, which is empty for one a person asked for, whose task was left in
// the state left before the restart.
func openRetainedRecovery(t *testing.T, origin string, left task.State) cancelledRecovery {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	tasks, err := task.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := state.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := agent.NewCatalog(map[string]agent.Config{"worker": {Harness: "test", Node: "node-a", Default: true}})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := harness.NewManager(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Stop)
	projects := project.Open(book)
	if err := projects.Declare(t.Context(), []project.Project{{ID: "p", Home: project.Home{Path: t.TempDir()}}, {ID: "p2", Home: project.Home{Path: t.TempDir()}}}); err != nil {
		t.Fatal(err)
	}
	attempts := attempt.New(book)
	coordinator := turntest.Unwired(t, func(o *turntest.Options) {
		o.Ledger, o.Catalog, o.Store, o.Runtime, o.Timeout = book, catalog, sessions, manager, 10*time.Second
		o.Tasks, o.Node, o.Executions, o.Attempts = tasks, "hub", execution.New(t.Context(), tasks), attempts
		o.Projects, o.DefaultProject = projects, "p"
		o.ConsoleCompletionGuard = console.CheckTaskCompletionTx
	})
	f := cancelledRecovery{coordinator: coordinator, tasks: tasks, sessions: sessions, projects: projects, taskID: seedCancelledRecovery(t, book, tasks, attempts, origin)}
	if left != task.StateRunning {
		if _, err := tasks.SetAside(f.taskID, left); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := attempts.PrepareRecovery(t.Context(), "startup"); err != nil {
		t.Fatal(err)
	}
	lifetime, stop := context.WithCancel(t.Context())
	f.cons = console.New(coordinator, "owner", nil)
	f.cons.SetRecoveryQuiet(0)
	f.cons.EnableRetainedRecovery(lifetime)
	if err := f.cons.PersistLedger(book); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stop()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := f.cons.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	workers := &reconciliationWorkers{}
	t.Cleanup(workers.Close)
	channels, err := assembleChannels(
		&runtimeValues{book: book, cfg: &config.Config{}, ctx: t.Context()},
		&ledgerValues{},
		&executionValues{coordinator: coordinator, gw: gateway.New(coordinator), catalogText: i18n.New(i18n.LocaleEN)},
		&readModelValues{}, &consoleValues{cons: f.cons, reconciliations: workers},
		&administrationValues{}, &delegationValues{},
	)
	if err != nil {
		t.Fatal(err)
	}
	coordinator.Wire(turntest.Callbacks(coordinatorCallbacks(nil, nil, messagingCallbacks{}, channels.Routes())))
	tasks.SetObserver(taskObserver(tasks, f.cons, func(string) {}))
	if err := f.cons.RecoverChats(lifetime, coordinator); err != nil {
		t.Fatal(err)
	}
	return f
}

// seedCancelledRecovery records a console turn opened by origin whose
// native command was accepted on node-a before the restart, and the
// exchange still waiting for it. It returns the turn's task.
func seedCancelledRecovery(t *testing.T, book *ledger.Ledger, tasks *task.Store, attempts *attempt.Service, origin string) string {
	t.Helper()
	tracked, err := tasks.Create(task.Task{Transport: "console", Channel: cancelledRecoveryConversation, Member: "worker", Requester: "owner", ProjectID: "p", Origin: origin, Goal: "original goal"})
	if err != nil {
		t.Fatal(err)
	}
	address := channel.Address{Channel: "console", Conversation: cancelledRecoveryConversation, Message: console.AnchorMark + "e1"}
	if _, err := tasks.BeginTurn(tracked.ID, "worker", "node-a", task.TurnInput{Address: address, ChatID: console.ChatID, ChatType: "p2p"}); err != nil {
		t.Fatal(err)
	}
	token, err := tasks.ExecutionToken(tracked.ID)
	if err != nil {
		t.Fatal(err)
	}
	r, err := attempts.Open(t.Context(), attempt.Spec{ID: "attempt-1", TaskID: tracked.ID, TurnID: address.Message, Kind: attempt.KindChat, Agent: "worker", Node: "node-a", Harness: "test", Project: "p", Workspace: project.Workspace{ID: "w", Project: "p", Node: "node-a", Path: t.TempDir(), Kind: project.KindCanonical}, Scope: attempt.ScopeUnrestricted, Execution: &token})
	if err != nil {
		t.Fatal(err)
	}
	if err := tasks.BindAttempt(token, r.ID, r.TurnID); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []attempt.State{attempt.Prepared, attempt.Running} {
		if r, err = attempts.Advance(t.Context(), r.ID, phase, "test", func(r *attempt.Record) { r.Session = "ns_original" }); err != nil {
			t.Fatal(err)
		}
	}
	e := consoleapi.Exchange{ID: "e1", Conversation: cancelledRecoveryConversation, Input: "original goal", Requester: "owner", Origin: origin, State: consoleapi.ExchangeRecovering, EnqueuedAt: time.Now(), StartedAt: time.Now()}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error {
		return console.StoreStateTx(tx, console.DurableState{Exchanges: map[string][]console.DurableExchange{cancelledRecoveryConversation: {{Exchange: e}}}})
	}); err != nil {
		t.Fatal(err)
	}
	return tracked.ID
}

// pendingOffering is the exchange's open question that offers choice.
func (f cancelledRecovery) pendingOffering(choice string) (consoleapi.PendingQuestion, bool) {
	for _, q := range f.cons.Questions(cancelledRecoveryConversation) {
		if q.ExchangeID != "e1" || q.State != "pending" {
			continue
		}
		for _, option := range q.Options {
			if option.ID == choice {
				return q, true
			}
		}
	}
	return consoleapi.PendingQuestion{}, false
}

// awaitOffering waits until the exchange asks a question that offers choice.
func (f cancelledRecovery) awaitOffering(t *testing.T, choice string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, ok := f.pendingOffering(choice); ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("recovery never offered %q: questions %+v, exchange %+v", choice, f.cons.Questions(cancelledRecoveryConversation), f.exchange())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// awaitStopWait waits until the exchange waits on the stop of the task's
// original execution, and fails at once if it offers to retry the task in
// the meantime.
func (f cancelledRecovery) awaitStopWait(t *testing.T) consoleapi.PendingQuestion {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if q, dead := f.pendingOffering("retry"); dead {
			t.Fatalf("recovery of task #%s offered to retry it: %+v", f.taskID, q)
		}
		if q, waiting := f.pendingOffering("recheck"); waiting {
			return q
		}
		if time.Now().After(deadline) {
			t.Fatalf("recovery never waited on the stop: questions %+v, exchange %+v", f.cons.Questions(cancelledRecoveryConversation), f.exchange())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (f cancelledRecovery) exchange() consoleapi.Exchange {
	for _, e := range f.cons.Queue(cancelledRecoveryConversation) {
		if e.ID == "e1" {
			return e
		}
	}
	return consoleapi.Exchange{}
}

// A task cancelled from the conversation can no longer be resumed, so a
// recovery question that offers to retry it has become one no answer can
// satisfy. Cancelling the task settles that question: the exchange moves on
// to confirming the stop of the task's execution.
func TestTaskCancelSettlesTheRecoveryQuestionOfTheCancelledTask(t *testing.T) {
	f := openCancelledRecovery(t)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, ok := f.pendingOffering("retry"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("recovery never asked the owner: questions %+v, exchange %+v", f.cons.Questions(cancelledRecoveryConversation), f.exchange())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := f.cons.SendCommand(t.Context(), cancelledRecoveryConversation, "/tasks cancel "+f.taskID, "cancel-1"); err != nil {
		t.Fatal(err)
	}
	if tracked, _ := f.tasks.Get(f.taskID); tracked.State != task.StateCancelled {
		t.Fatalf("task after /tasks cancel = %s, want cancelled", tracked.State)
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		_, dead := f.pendingOffering("retry")
		_, waiting := f.pendingOffering("recheck")
		if !dead && waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("recovery question of cancelled task #%s is unsettled: offers retry %v, stop wait shown %v; questions %+v", f.taskID, dead, waiting, f.cons.Questions(cancelledRecoveryConversation))
		}
		time.Sleep(5 * time.Millisecond)
	}
	// The node holding the execution is away, so the stop cannot be
	// confirmed yet: the exchange keeps its place instead of closing.
	if e := f.exchange(); e.State != consoleapi.ExchangeAwaitingUser {
		t.Fatalf("exchange while the stop is unconfirmed = %+v, want awaiting user", e)
	}
}

// A task cancelled or paused while Steve was down is known to the task
// store alone, and the restarted recovery goes by it: it never offers to
// retry a task nothing may resume, and waits on the stop of the task's
// original execution instead, saying whether the task was cancelled or
// paused. Trying the stop leaves the task as it was set aside.
func TestRestartIntoASetAsideTaskWaitsOnTheStop(t *testing.T) {
	zh := i18n.New(i18n.LocaleZH)
	for _, tc := range []struct {
		state task.State
		says  i18n.Key
	}{
		{task.StateCancelled, i18n.ConsoleStopRecorded},
		{task.StatePaused, i18n.ConsolePauseRecorded},
	} {
		t.Run(string(tc.state), func(t *testing.T) {
			f := openRetainedRecovery(t, "", tc.state)
			card := f.awaitStopWait(t)
			if !strings.Contains(card.Message, zh.T(tc.says)) {
				t.Fatalf("the stop wait of a %s task says %q, want %q", tc.state, card.Message, zh.T(tc.says))
			}
			if e := f.exchange(); e.State != consoleapi.ExchangeAwaitingUser {
				t.Fatalf("exchange while the stop is unconfirmed = %+v, want awaiting user", e)
			}
			if tracked, _ := f.tasks.Get(f.taskID); tracked.State != tc.state {
				t.Fatalf("task while its stop is unconfirmed = %s, want %s", tracked.State, tc.state)
			}
		})
	}
}

func TestEveryTaskSetAsideWakesItsRecovery(t *testing.T) {
	for _, state := range []task.State{task.StatePaused, task.StateCancelled} {
		t.Run(string(state), func(t *testing.T) {
			f := openCancelledRecovery(t)
			deadline := time.Now().Add(5 * time.Second)
			for {
				if _, ok := f.pendingOffering("retry"); ok {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("no recovery question")
				}
				time.Sleep(time.Millisecond)
			}
			if _, err := f.tasks.SetAside(f.taskID, state); err != nil {
				t.Fatal(err)
			}
			deadline = time.Now().Add(2 * time.Second)
			for {
				if _, ok := f.pendingOffering("recheck"); ok {
					return
				}
				if time.Now().After(deadline) {
					t.Fatalf("%s did not wake recovery", state)
				}
				time.Sleep(time.Millisecond)
			}
		})
	}
}
