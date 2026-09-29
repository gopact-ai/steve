package app

import (
	"context"
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
	"github.com/gopact-ai/steve/internal/turn/turntest"
)

const cancelledRecoveryConversation = "console:main"

type cancelledRecovery struct {
	tasks  *task.Store
	cons   *console.Service
	taskID string
}

// openCancelledRecovery restarts into a console exchange whose original
// execution is retained on a node that is not connected, with the
// production channels and callbacks wired. Its recovery puts the block to
// the owner at once.
func openCancelledRecovery(t *testing.T) cancelledRecovery {
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
	if err := projects.Declare(t.Context(), []project.Project{{ID: "p", Home: project.Home{Path: t.TempDir()}}}); err != nil {
		t.Fatal(err)
	}
	attempts := attempt.New(book)
	coordinator := turntest.Unwired(t, func(o *turntest.Options) {
		o.Ledger, o.Catalog, o.Store, o.Runtime, o.Timeout = book, catalog, sessions, manager, 10*time.Second
		o.Tasks, o.Node, o.Executions, o.Attempts = tasks, "hub", execution.New(t.Context(), tasks), attempts
		o.Projects, o.DefaultProject = projects, "p"
	})
	f := cancelledRecovery{tasks: tasks, taskID: seedCancelledRecovery(t, book, tasks, attempts)}
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
	if err := f.cons.RecoverChats(lifetime, coordinator); err != nil {
		t.Fatal(err)
	}
	return f
}

// seedCancelledRecovery records a console turn whose native command was
// accepted on node-a before the restart, and the exchange still waiting
// for it. It returns the turn's task.
func seedCancelledRecovery(t *testing.T, book *ledger.Ledger, tasks *task.Store, attempts *attempt.Service) string {
	t.Helper()
	tracked, err := tasks.Create(task.Task{Transport: "console", Channel: cancelledRecoveryConversation, Member: "worker", Requester: "owner", ProjectID: "p", Goal: "original goal"})
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
	e := consoleapi.Exchange{ID: "e1", Conversation: cancelledRecoveryConversation, Input: "original goal", Requester: "owner", State: consoleapi.ExchangeRecovering, EnqueuedAt: time.Now(), StartedAt: time.Now()}
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
