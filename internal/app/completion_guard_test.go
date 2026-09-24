package app

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/agentexec"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/console"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/exec"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/httpapi"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/readmodel"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turn"
	"github.com/gopact-ai/steve/internal/turn/turntest"
)

type completionFixture struct {
	coordinator *turn.Coordinator
	book        *ledger.Ledger
	tasks       *task.Store
	root        task.Task
}

// Assemble the actual console boundary, but never start a harness, listener,
// recovery worker or config loader. Every durable resource is temporary.
func assembledCompletion(t *testing.T) completionFixture {
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
	catalog, err := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Default: true}})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := harness.NewManager(nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(t.Context())
	t.Cleanup(stop)
	assembler := capability.NewAssembler(nil)
	attempts := attempt.New(book)
	registry := execution.New(ctx, tasks)
	coordinator := turntest.New(t, func(o *turntest.Options) {
		o.Ledger, o.Catalog, o.Store, o.Assembler, o.Runtime, o.Timeout = book, catalog, sessions, assembler, manager, time.Minute
		o.Tasks, o.Node, o.Attempts, o.Executions = tasks, "test", attempts, registry
	})
	dir := t.TempDir()
	cfg := &config.Config{Gateway: config.Gateway{StatePath: filepath.Join(dir, "state.json"), OwnerID: "test-owner"}}
	life := &applicationLifetime{}
	t.Cleanup(func() {
		if err := life.Close(); err != nil {
			t.Error(err)
		}
	})
	_, err = assembleConsole(life,
		&assemblyInput{parent: ctx, path: filepath.Join(dir, "unused-config.json"), environment: &Environment{HTTPConfig: &httpapi.ServerConfig{Addr: "127.0.0.1:0", Token: "completion-guard"}}},
		&runtimeValues{book: book, catalog: catalog, cfg: cfg, ctx: ctx, manager: manager, nodeName: "test", stop: stop},
		&ledgerValues{attempts: attempts, store: sessions},
		&homeValues{assembler: assembler},
		&fleetValues{projects: project.Open(book)},
		&executionValues{coordinator: coordinator, executions: registry, tasks: tasks},
		&plansValues{auxiliary: &agentexec.Runner{}, stepRunner: &exec.AgentRunner{}},
		&readModelValues{view: readmodel.New(readmodel.Sources{})},
	)
	if err != nil {
		t.Fatal(err)
	}
	root, err := tasks.Create(task.Task{Transport: "console", Channel: "chat", Member: "codex", Goal: "accepted work"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Begin(root.ID, "codex", "test", "native-session"); err != nil {
		t.Fatal(err)
	}
	root, err = tasks.Finish(root.ID, task.OutcomeOK, task.Tokens{Total: 17}, 2)
	if err != nil {
		t.Fatal(err)
	}
	return completionFixture{coordinator: coordinator, book: book, tasks: tasks, root: root}
}

func (f completionFixture) complete(t *testing.T, exchange string, want bool) {
	t.Helper()
	beforeStore, err := task.OpenLedger(f.book)
	if err != nil {
		t.Fatal(err)
	}
	durableBefore, found := beforeStore.Get(f.root.ID)
	if !found || len(durableBefore.Attempts) == 0 {
		t.Fatal("completion fixture lacks durable task accounting")
	}
	result, err := f.coordinator.Handle(t.Context(), turn.Request{
		ConversationID: f.root.Channel, Channel: "console", ExchangeID: exchange, Locale: "en",
		Input: "/tasks complete " + f.root.ID,
	})
	after, _ := f.tasks.Get(f.root.ID)
	if (err == nil) != want || after.CompletedByUser != want || strings.Contains(result.Text, "is completed") != want {
		t.Fatalf("completion: want=%v root=%+v result=%+v err=%v", want, after, result, err)
	}
	reopened, err := task.OpenLedger(f.book)
	if err != nil {
		t.Fatal(err)
	}
	stored, found := reopened.Get(f.root.ID)
	if !found {
		t.Fatal("completion lost durable task")
	}
	if want {
		if stored.State != task.StateDone || !stored.CompletedByUser || stored.ExecutionEpoch != f.root.ExecutionEpoch+1 {
			t.Fatalf("completion not durable or did not revoke epoch: %+v", stored)
		}
	} else {
		if !reflect.DeepEqual(f.root, after) || !reflect.DeepEqual(durableBefore, stored) {
			t.Fatal("refusal changed state, accounting or epoch")
		}
	}
}

func (f completionFixture) saveConsole(t *testing.T, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var state console.DurableState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	for id, q := range state.Questions {
		q.ID = id
		state.Questions[id] = q
	}
	if err := f.book.Update(t.Context(), func(tx *ledger.Tx) error { return console.StoreStateTx(tx, state) }); err != nil {
		t.Fatal(err)
	}
}

func TestCompletionExemptsOnlyItsOwnConsoleExchange(t *testing.T) {
	for _, scenario := range []string{"own", "queued-completion", "running-completion", "missing-identity", "terminal-completion"} {
		t.Run(scenario, func(t *testing.T) {
			f := assembledCompletion(t)
			current := consoleapi.Exchange{ID: "current", Conversation: "chat", Input: "/tasks complete 1", State: consoleapi.ExchangeRunning}
			exchanges := []consoleapi.Exchange{current}
			if scenario != "own" && scenario != "missing-identity" {
				other := consoleapi.Exchange{ID: "other", Conversation: "chat", Input: "/tasks complete 2", State: consoleapi.ExchangeQueued}
				if scenario == "running-completion" {
					other.State = consoleapi.ExchangeRunning
				} else if scenario == "terminal-completion" {
					other.State = consoleapi.ExchangeDone
				}
				exchanges = append(exchanges, other)
			}
			f.saveConsole(t, map[string]any{"exchanges": map[string][]consoleapi.Exchange{"chat": exchanges}})
			currentID := current.ID
			if scenario == "missing-identity" {
				currentID = ""
			}
			f.complete(t, currentID, scenario == "own" || scenario == "terminal-completion")
		})
	}
}

func TestTaskCompletionDurableGuardRefusesConsoleFacts(t *testing.T) {
	for _, scenario := range []string{"question", "recovery", "continuation", "reply-pending", "queued-input", "corrupt", "corrupt-owner-field"} {
		t.Run(scenario, func(t *testing.T) {
			f := assembledCompletion(t)
			switch scenario {
			case "question":
				f.saveConsole(t, map[string]any{"questions": map[string]consoleapi.PendingQuestion{"q": {TaskID: f.root.ID, Conversation: "chat", State: "pending"}}})
			case "recovery", "continuation":
				exchange := consoleapi.Exchange{ID: "old", Conversation: "chat", ExpectedTask: f.root.ID, State: consoleapi.ExchangeQueued}
				if scenario == "recovery" {
					exchange.ExpectedTask = ""
					exchange.State = consoleapi.ExchangeAwaitingUser
				}
				f.saveConsole(t, map[string]any{"exchanges": map[string][]consoleapi.Exchange{"chat": {exchange}}})
			case "reply-pending", "queued-input":
				exchange := consoleapi.Exchange{ID: "original", Input: "original user work", Conversation: "chat", State: consoleapi.ExchangeRunning}
				if scenario == "queued-input" {
					exchange.State = consoleapi.ExchangeQueued
				}
				f.saveConsole(t, map[string]any{"exchanges": map[string][]consoleapi.Exchange{"chat": {exchange}}})
			case "corrupt", "corrupt-owner-field":
				raw := "invalid"
				if scenario == "corrupt-owner-field" {
					raw = `{"revision":0}`
				}
				if err := func() error {
					_, err := f.book.DB().Exec(`INSERT INTO bindings(kind,id,data,updated_at) VALUES('console-store','state',?,'now') ON CONFLICT(kind,id) DO UPDATE SET data=excluded.data`, raw)
					return err
				}(); err != nil {
					t.Fatal(err)
				}
			}
			f.complete(t, "", false)
		})
	}
}

func TestCompletionDoesNotBlockOnItsOwnDurableCommand(t *testing.T) {
	f := assembledCompletion(t)
	exchanges := map[string][]consoleapi.Exchange{"chat": {
		{ID: "original", Conversation: "chat", Input: "work", State: consoleapi.ExchangeDone},
		{ID: "complete", Conversation: "chat", Input: "@codex /tasks complete 1", State: consoleapi.ExchangeRunning},
	}}
	f.saveConsole(t, map[string]any{"exchanges": exchanges})
	f.complete(t, "complete", true)
}

func TestCompletionOwnerGuardSharesTaskWriteTransaction(t *testing.T) {
	for _, stage := range []string{"guard-refusal", "task-write-failure"} {
		t.Run(stage, func(t *testing.T) {
			f := assembledCompletion(t)
			child, err := f.tasks.Spawn(f.root.ID, task.Task{Member: "child"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.tasks.SetAside(child.ID, task.StateCancelled); err != nil {
				t.Fatal(err)
			}
			if stage == "task-write-failure" {
				if _, err := f.book.DB().Exec(`CREATE TRIGGER reject_completion BEFORE UPDATE ON bindings
					WHEN NEW.kind = 'task-store' AND NEW.id = 'state'
					BEGIN SELECT RAISE(ABORT, 'completion write unavailable'); END`); err != nil {
					t.Fatal(err)
				}
			}
			called := false
			f.coordinator.SetConsoleCompletionGuard(func(tx *ledger.Tx, ids map[string]bool, conversation, currentExchange string) error {
				called = true
				if !ids[f.root.ID] || !ids[child.ID] || conversation != "chat" || currentExchange != "current" {
					t.Errorf("guard lost completion identity: ids=%v conversation=%q exchange=%q", ids, conversation, currentExchange)
				}
				questions := map[string]consoleapi.PendingQuestion{}
				if stage == "guard-refusal" {
					questions["child"] = consoleapi.PendingQuestion{ID: "child", TaskID: child.ID, State: "pending"}
				}
				if err := console.StoreStateTx(tx, console.DurableState{Questions: questions}); err != nil {
					return err
				}
				// This is the real owner, reading a fact visible only in tx.
				return console.CheckTaskCompletionTx(tx, ids, conversation, currentExchange)
			})
			f.complete(t, "current", false)
			if !called {
				t.Fatal("task completion did not call the injected owner")
			}
			if state, err := console.LoadState(f.book); err != nil || len(state.Questions) > 0 {
				t.Fatalf("failed completion committed owner facts: %+v err=%v", state, err)
			}
			if after, _ := f.tasks.Get(child.ID); after.ExecutionEpoch != child.ExecutionEpoch+1 {
				t.Fatalf("failed completion changed child's cancellation epoch: %+v", after)
			}
		})
	}
}
