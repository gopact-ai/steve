package turn

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
)

func completionCoordinator(t *testing.T, runner *fakeRunner) (*Coordinator, *ledger.Ledger) {
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
	sessions, err := state.OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Default: true}})
	coordinator := New(catalog, sessions, capability.NewAssembler(nil), &fakeManager{runners: map[string]*fakeRunner{"codex": runner}}, time.Minute)
	coordinator.text = i18n.New(i18n.LocaleEN)
	coordinator.SetProjects(projects, "p", "")
	coordinator.SetTasks(tasks, "hub")
	coordinator.SetAttempts(attempt.New(book))
	artifacts := artifact.New(filepath.Join(t.TempDir(), "artifacts"), book, projects, artifact.LocalNodes{Dir: t.TempDir()})
	coordinator.SetArtifacts(artifacts)
	registry := execution.New(t.Context(), tasks)
	coordinator.SetExecution(registry)
	artifacts.SetExecution(registry)
	return coordinator, book
}

func TestCompleteTaskThenChatKeepsNativeContextAndNeverResumesClosedRoot(t *testing.T) {
	runner := &fakeRunner{id: "native-context", reply: "accepted"}
	coordinator, book := completionCoordinator(t, runner)
	if _, err := handle(coordinator, t.Context(), "first work"); err != nil {
		t.Fatal(err)
	}
	before := coordinator.store.Conversation("chat")
	tracked, _ := coordinator.tasks.Get("1")
	if tracked.State != task.StateRunning {
		t.Fatal("turn automatically completed the task")
	}
	for _, input := range []string{"/tasks complete 1", "/tasks done 1", "/tasks 完成 1", "/tasks complete"} {
		result, err := handle(coordinator, t.Context(), input)
		if err != nil || !strings.Contains(result.Text, "#1") || !strings.Contains(result.Text, "completed") {
			t.Fatalf("%s: %+v %v", input, result, err)
		}
	}
	if !reflect.DeepEqual(before, coordinator.store.Conversation("chat")) {
		t.Fatal("completion changed session state")
	}
	closed, _ := coordinator.tasks.Get("1")
	reopened, err := task.OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	if stored, _ := reopened.Get("1"); stored.State != task.StateDone {
		t.Fatal("completion not durable")
	}
	resumes := 0
	coordinator.SetResumer(func(TaskResume) { resumes++ })
	if _, err := handle(coordinator, t.Context(), "/tasks resume 1"); err != nil {
		t.Fatal(err)
	}
	if resumes != 0 {
		t.Fatal("closed task was scheduled for resume")
	}
	if _, err := coordinator.Handle(t.Context(), Request{ConversationID: "chat", Input: "old continuation", ExpectedTask: "1"}); !errors.Is(err, task.ErrContinuationUnavailable) {
		t.Fatalf("old continuation: %v", err)
	}
	if _, err := handle(coordinator, t.Context(), "next work"); err != nil {
		t.Fatal(err)
	}
	if len(coordinator.tasks.List("chat")) != 2 || len(runner.seen()) != 2 {
		t.Fatal("new message did not open exactly one new task")
	}
	if after, _ := coordinator.tasks.Get("1"); !reflect.DeepEqual(closed, after) {
		t.Fatal("old continuation or new chat wrote closed root")
	}
	if after := coordinator.store.Conversation("chat"); after.Sessions["codex"].UpstreamID != before.Sessions["codex"].UpstreamID {
		t.Fatal("native session changed")
	}
}

func TestCompleteTaskChecksConversationEvenOnRetry(t *testing.T) {
	coordinator, _ := completionCoordinator(t, &fakeRunner{reply: "ok"})
	if _, err := handle(coordinator, t.Context(), "work"); err != nil {
		t.Fatal(err)
	}
	for _, completed := range []bool{false, true} {
		if completed {
			if _, err := handle(coordinator, t.Context(), "/tasks complete 1"); err != nil {
				t.Fatal(err)
			}
		}
		result, err := coordinator.Handle(t.Context(), Request{ConversationID: "foreign", Input: "/tasks complete 1"})
		if err == nil || !strings.Contains(result.Text, "no task #1") {
			t.Fatalf("ownership: %+v %v", result, err)
		}
	}
}

func TestCompleteTaskRejectsActiveTurnAndRegistryReservation(t *testing.T) {
	runner := &fakeRunner{reply: "ok", started: make(chan struct{}), done: make(chan struct{})}
	coordinator, _ := completionCoordinator(t, runner)
	finished := make(chan error, 1)
	go func() { _, err := handle(coordinator, t.Context(), "work"); finished <- err }()
	<-runner.started
	if result, err := handle(coordinator, t.Context(), "/tasks complete 1"); err == nil || !strings.Contains(result.Text, "execution") {
		t.Fatalf("busy: %+v %v", result, err)
	}
	close(runner.done)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	scope, err := coordinator.executions.Begin(t.Context(), execution.Key{TaskID: "1", InstanceID: "reserved"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handle(coordinator, t.Context(), "/tasks complete 1"); err == nil {
		t.Fatal("completed reserved registry scope")
	}
	scope.Finish(errors.New("unknown execution"))
	if _, err := handle(coordinator, t.Context(), "/tasks complete 1"); err == nil {
		t.Fatal("completed unresolved registry scope")
	}
}

func TestTaskCompletionDurableGuardRefusesPendingFacts(t *testing.T) {
	for _, scenario := range []string{"reserved", "unknown", "unsettled", "question", "recovery", "continuation", "reply-pending", "queued-input", "landing", "effect", "disclosure", "plan", "corrupt"} {
		t.Run(scenario, func(t *testing.T) {
			coordinator, book := completionCoordinator(t, &fakeRunner{reply: "ok"})
			if _, err := handle(coordinator, t.Context(), "accepted"); err != nil {
				t.Fatal(err)
			}
			token, _ := coordinator.tasks.ExecutionToken("1")
			writeDoc := func(name string, value any) {
				raw, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				if err := book.Document(name).Save(raw); err != nil {
					t.Fatal(err)
				}
			}
			begin := func(kind, state string, value any) {
				if _, err := book.Begin(t.Context(), "pending", kind, state, "test", value); err != nil {
					t.Fatal(err)
				}
			}
			switch scenario {
			case "reserved", "unknown", "unsettled":
				state := "leased"
				if scenario != "reserved" {
					state = "bound"
				}
				begin("attempt", state, attempt.Record{Spec: attempt.Spec{ID: "pending", TaskID: "1"}, Unsettled: scenario == "unsettled"})
			case "question":
				writeDoc("console", map[string]any{"questions": map[string]consoleapi.PendingQuestion{"q": {TaskID: "1", Conversation: "chat", State: "pending"}}})
			case "recovery", "continuation":
				exchange := consoleapi.Exchange{ID: "old", Conversation: "chat", ExpectedTask: "1", State: consoleapi.ExchangeQueued}
				if scenario == "recovery" {
					exchange.ExpectedTask = ""
					exchange.State = consoleapi.ExchangeAwaitingUser
				}
				writeDoc("console", map[string]any{"exchanges": map[string][]consoleapi.Exchange{"chat": {exchange}}})
			case "reply-pending", "queued-input":
				exchange := consoleapi.Exchange{ID: "original", Input: "original user work", Conversation: "chat", State: consoleapi.ExchangeRunning}
				if scenario == "queued-input" {
					exchange.State = consoleapi.ExchangeQueued
				}
				writeDoc("console", map[string]any{"exchanges": map[string][]consoleapi.Exchange{"chat": {exchange}}})
			case "landing":
				begin("landing", "proposed", artifact.Landing{Source: &artifact.Source{Execution: &token}})
			case "effect":
				begin("intent", "outcome-unknown", map[string]string{"task_id": "1"})
			case "disclosure":
				begin("disclosure-request", "proposed", project.DisclosureRequest{TaskID: "1"})
			case "plan":
				writeDoc("plans", map[string]any{"by_task": map[string]string{"1": "plan"}})
			case "corrupt":
				if err := book.Document("console").Save([]byte("invalid")); err != nil {
					t.Fatal(err)
				}
			}
			if result, err := handle(coordinator, t.Context(), "/tasks complete 1"); err == nil || strings.Contains(result.Text, "is completed") {
				t.Fatalf("%s: %+v %v", scenario, result, err)
			}
			if tracked, _ := coordinator.tasks.Get("1"); tracked.State != task.StateRunning || tracked.ExecutionEpoch != token.Epoch {
				t.Fatal("refusal changed state or epoch")
			}
		})
	}
}

func TestCompleteAliasesAreImmediateControls(t *testing.T) {
	for _, input := range []string{"/tasks complete 1", "/tasks done", "/tasks 完成 #1", "@codex /tasks complete 1"} {
		if !ImmediateInput(input) {
			t.Fatalf("completion queued behind active work: %s", input)
		}
	}
}

func TestCompletionDoesNotBlockOnItsOwnDurableCommand(t *testing.T) {
	coordinator, book := completionCoordinator(t, &fakeRunner{reply: "ok"})
	if _, err := handle(coordinator, t.Context(), "work"); err != nil {
		t.Fatal(err)
	}
	exchanges := map[string][]consoleapi.Exchange{"chat": {
		{ID: "original", Conversation: "chat", Input: "work", State: consoleapi.ExchangeDone},
		{ID: "complete", Conversation: "chat", Input: "@codex /tasks complete 1", State: consoleapi.ExchangeRunning},
	}}
	raw, err := json.Marshal(map[string]any{"exchanges": exchanges})
	if err != nil {
		t.Fatal(err)
	}
	if err := book.Document("console").Save(raw); err != nil {
		t.Fatal(err)
	}
	if _, err := handle(coordinator, t.Context(), "/tasks complete 1"); err != nil {
		t.Fatal(err)
	}
}
