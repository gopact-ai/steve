package turn

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/datalevel"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
)

func completionCoordinator(t *testing.T, runner *fakeRunner, opts ...testOption) (*Coordinator, *ledger.Ledger) {
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
	tasks, err := task.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := state.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	catalog, _ := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Default: true}})
	coordinator := buildCoordinator(t, append([]testOption{withDeps(func(d *Deps) {
		d.Catalog, d.Store, d.Assembler, d.Runtime, d.Timeout = catalog, sessions, capability.NewAssembler(nil), &fakeManager{runners: map[string]*fakeRunner{"codex": runner}}, time.Minute
		d.Projects, d.DefaultProject = projects, "p"
		d.Tasks, d.Node, d.Text = tasks, "hub", i18n.New(i18n.LocaleEN)
	}), onLedger(book)}, opts...)...)
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
	reopened, err := task.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	if stored, _ := reopened.Get("1"); stored.State != task.StateDone {
		t.Fatal("completion not durable")
	}
	resumes := 0
	coordinator.SetResumer(func(TaskResume) error { resumes++; return nil })
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

func TestBareCompletionSkipsNewerTerminalRoots(t *testing.T) {
	t.Parallel()
	for _, state := range []task.State{task.StateDone, task.StateFailed, task.StateCancelled, task.StatePaused} {
		t.Run(string(state), func(t *testing.T) {
			c, _ := completionCoordinator(t, &fakeRunner{reply: "accepted"})
			if _, err := handle(c, t.Context(), "older accepted work"); err != nil {
				t.Fatal(err)
			}
			newer, err := c.tasks.Create(task.Task{Channel: "chat", Member: "other", ProjectID: "p"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.tasks.Advance(newer.ID, state); err != nil {
				t.Fatal(err)
			}
			result, err := handle(c, t.Context(), "/tasks complete")
			if err != nil || !strings.Contains(result.Text, "#1") {
				t.Fatalf("bare completion selected terminal root %s: %+v %v", newer.ID, result, err)
			}
			if root, _ := c.tasks.Get("1"); !root.CompletedByUser {
				t.Fatal("older accepted root was left open")
			}
			if unchanged, _ := c.tasks.Get(newer.ID); unchanged.State != state {
				t.Fatal("bare completion changed the skipped root")
			}
		})
	}
}

func TestDisclosurePersistenceErrorDoesNotRewriteSuccessfulAgentOutcome(t *testing.T) {
	c, book := completionCoordinator(t, &fakeRunner{reply: "sealed answer"})
	p, _, err := c.projects.Get(t.Context(), "p")
	if err != nil {
		t.Fatal(err)
	}
	p.Level, p.DefaultRole = datalevel.Sealed, project.RoleWrite
	if err := c.projects.Declare(t.Context(), []project.Project{p}); err != nil {
		t.Fatal(err)
	}
	if _, err := book.DB().Exec(`CREATE TRIGGER reject_disclosure BEFORE INSERT ON operations WHEN NEW.kind='disclosure-request' BEGIN SELECT RAISE(ABORT, 'disclosure unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	result, err := c.Handle(t.Context(), Request{ConversationID: "chat", SenderOpenID: "guest", Input: "work", MessageID: "sealed-turn"})
	if err == nil || !strings.Contains(err.Error(), "disclosure unavailable") || result.Text != "" {
		t.Fatalf("disclosure failure was hidden or answer leaked: %+v %v", result, err)
	}
	root, ok := c.tasks.Get("1")
	if !ok || len(root.Attempts) != 1 || root.Attempts[0].Open() || root.Attempts[0].Outcome != task.OutcomeOK {
		t.Fatalf("disclosure persistence rewrote successful execution: %+v", root.Attempts)
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

func TestCancelledChildWithoutResultSettlesOnlyAfterItsExecutionStops(t *testing.T) {
	c, _ := completionCoordinator(t, &fakeRunner{reply: "accepted"})
	if _, err := handle(c, t.Context(), "work"); err != nil {
		t.Fatal(err)
	}
	child, err := c.tasks.Spawn("1", task.Task{Member: "child"})
	if err != nil {
		t.Fatal(err)
	}
	scope, err := c.executions.Begin(t.Context(), execution.Key{TaskID: child.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.tasks.SetAside(child.ID, task.StateCancelled); err != nil {
		t.Fatal(err)
	}
	if _, err := handle(c, t.Context(), "/tasks complete 1"); err == nil {
		t.Fatal("cancelled child still had an active execution")
	}
	scope.Finish(nil)
	if _, err := handle(c, t.Context(), "/tasks complete 1"); err != nil {
		t.Fatal(err)
	}
	closed, _ := c.tasks.Get(child.ID)
	if closed.State != task.StateCancelled || closed.Result != nil || closed.Delivery != nil {
		t.Fatal("parent completion invented a child result or delivery")
	}
}

func TestTaskCompletionDurableGuardRefusesPendingFacts(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"reserved", "unknown", "unsettled", "landing", "effect", "disclosure", "plan"} {
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
			case "landing":
				begin("landing", "proposed", artifact.Landing{Source: &artifact.Source{Execution: &token}})
			case "effect":
				begin("intent", "outcome-unknown", map[string]string{"task_id": "1"})
			case "disclosure":
				begin("disclosure-request", "proposed", project.DisclosureRequest{TaskID: "1"})
			case "plan":
				writeDoc("plans", map[string]any{"by_task": map[string]string{"1": "plan"}})
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
