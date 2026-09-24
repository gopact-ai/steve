package app

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/console"
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

type resumeFixture struct {
	book        *ledger.Ledger
	tasks       *task.Store
	sessions    *state.Store
	coordinator *turn.Coordinator
	console     *console.Service
}

// Wire the production channel resumer, with real console and SQLite owners.
// No configuration loader, network channel, or native agent is started.
func assembledResume(t *testing.T) resumeFixture {
	return assembledResumeWithHarness(t, "")
}

func assembledResumeWithHarness(t *testing.T, command string) resumeFixture {
	return assembledResumeWithGate(t, command, nil)
}

func assembledResumeWithGate(t *testing.T, command string, gate *resumeInputGate) resumeFixture {
	return assembledResumeAt(t, t.TempDir(), command, gate)
}

// adjust changes the callbacks the channels built before they are wired.
func assembledResumeAt(t *testing.T, dir, command string, gate *resumeInputGate, adjust ...func(*turn.Callbacks)) resumeFixture {
	t.Helper()
	book, err := ledger.Open(dir, ledger.Options{})
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
	configs := map[string]harness.Config{}
	if command != "" {
		configs["codex"] = harness.Config{Command: command, Permission: "auto"}
	}
	manager, err := harness.NewManager(configs)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Stop)
	projects := project.Open(book)
	if _, exists, err := projects.Get(t.Context(), "p"); err != nil {
		t.Fatal(err)
	} else if !exists {
		if err := projects.Declare(t.Context(), []project.Project{{ID: "p", Home: project.Home{Path: t.TempDir()}}}); err != nil {
			t.Fatal(err)
		}
	}
	coordinator := turntest.Unwired(t, func(o *turntest.Options) {
		o.Ledger, o.Catalog, o.Store, o.Runtime, o.Timeout = book, catalog, sessions, manager, 10*time.Second
		o.Tasks, o.Node, o.Executions = tasks, "test", execution.New(t.Context(), tasks)
		o.Projects, o.DefaultProject, o.Attempts = projects, "p", attempt.New(book)
		o.Artifacts = artifact.New(t.TempDir(), book, projects, artifact.LocalNodes{Dir: t.TempDir()})
		o.ChannelOwners = map[string]string{"feishu": "owner", "unknown": "owner"}
	})
	var handler console.Handler = coordinator
	if gate != nil {
		gate.Coordinator = coordinator
		handler = gate
	}
	cons := console.New(handler, "owner", nil)
	if err := cons.PersistLedger(book); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := cons.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	workers := &reconciliationWorkers{}
	t.Cleanup(workers.Close)
	channels, err := assembleChannels(
		&runtimeValues{book: book, cfg: &config.Config{}, ctx: t.Context()},
		&ledgerValues{},
		&executionValues{coordinator: coordinator, gw: gateway.New(coordinator), catalogText: i18n.New(i18n.LocaleEN)},
		&readModelValues{}, &consoleValues{cons: cons, reconciliations: workers},
		&administrationValues{}, &delegationValues{},
	)
	if err != nil {
		t.Fatal(err)
	}
	callbacks := channels.Callbacks()
	for _, change := range adjust {
		change(&callbacks)
	}
	coordinator.Wire(turntest.Callbacks(callbacks))
	return resumeFixture{book, tasks, sessions, coordinator, cons}
}

func resumeAgent(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "mockagent")
	if output, err := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent").CombinedOutput(); err != nil {
		t.Fatalf("build isolated native test agent: %v\n%s", err, output)
	}
	return bin
}

func waitResumeReceipt(t *testing.T, f resumeFixture, conversation, id string) {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		for _, e := range f.console.Queue(conversation) {
			if e.ID == id && e.State.Terminal() {
				if e.State != "done" {
					t.Fatalf("resume exchange did not succeed: %+v; replies=%+v", e, f.console.Replies(conversation))
				}
				return
			}
		}
		select {
		case <-deadline:
			t.Fatal("resume receipt did not settle")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestTaskResumeStableControlIsIdempotentBeforeAndAfterConsumption(t *testing.T) {
	gate := &resumeInputGate{entered: make(chan turn.Request, 4), release: make(chan struct{})}
	f := assembledResumeWithGate(t, resumeAgent(t), gate)
	t.Cleanup(func() { close(gate.release) })
	before := f.paused(t, "console")
	command := "/tasks resume " + before.ID
	first, err := f.console.SendCommand(t.Context(), before.Channel, command, "control-id")
	if err != nil {
		t.Fatal(err)
	}
	var input turn.Request
	select {
	case input = <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("authorized input did not arrive")
	}
	granted, _ := f.tasks.Get(before.ID)
	for range 2 {
		replayed, err := f.console.SendCommand(t.Context(), before.Channel, command, "control-id")
		if err != nil || replayed.ID != first.ID {
			t.Fatalf("same control did not replay its original receipt: %+v, %v", replayed, err)
		}
	}
	// Also exercise manual orchestration itself, not only channel dedup.
	if _, err := f.coordinator.Handle(t.Context(), turn.Request{Channel: "console", ConversationID: before.Channel,
		ExchangeID: first.ExchangeID, MessageID: console.AnchorMark + first.ExchangeID, Input: command}); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.tasks.Get(before.ID); !reflect.DeepEqual(granted, got) {
		t.Fatal("same identity reauthorized or charged task")
	}
	if len(f.console.Queue(before.Channel)) != 2 {
		t.Fatal("repeated resume created extra durable input")
	}
	duplicateControl, err := f.console.EnqueueCommand(t.Context(), before.Channel, command, "another-click", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.coordinator.Handle(t.Context(), turn.Request{Channel: "console", ConversationID: before.Channel,
		MessageID: "another-control", Input: command}); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.tasks.Get(before.ID); !reflect.DeepEqual(granted, got) || len(f.console.Queue(before.Channel)) != 3 {
		t.Fatal("another resume click replaced the outstanding authorized input")
	}
	gate.release <- struct{}{}
	waitResumeReceipt(t, f, before.Channel, input.ExchangeID)
	waitResumeReceipt(t, f, before.Channel, duplicateControl.ID)
	consumed, _ := f.tasks.Get(before.ID)
	if !consumed.ResumeGrant.Consumed || consumed.ResumeGrant.TurnID != input.MessageID || consumed.Budget.Turns != before.Budget.Turns+1 {
		t.Fatalf("original grant was not consumed exactly once: %+v", consumed)
	}
	if _, err := f.coordinator.Handle(t.Context(), turn.Request{Channel: "console", ConversationID: before.Channel,
		ExchangeID: first.ExchangeID, MessageID: console.AnchorMark + first.ExchangeID, Input: command}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.console.SendCommand(t.Context(), before.Channel, command, "control-id"); err != nil {
		t.Fatal(err)
	}
	if err := f.console.Drain(); err != nil {
		t.Fatal(err)
	}
	native, err := attempt.New(f.book).ForTask(t.Context(), before.ID)
	if err != nil || len(native) != 1 || native[0].State != attempt.Bound || native[0].TurnID != input.MessageID {
		t.Fatalf("consumed retry replaced the original attempt: %+v, %v", native, err)
	}
	if got, _ := f.tasks.Get(before.ID); !reflect.DeepEqual(consumed, got) {
		t.Fatal("consumed replay changed accounting or authority")
	}
	select {
	case duplicate := <-gate.entered:
		t.Fatalf("consumed retry entered Handle again: %+v", duplicate)
	default:
	}
}

func TestTaskResumeDormantGrantSurvivesActualSQLiteReopen(t *testing.T) {
	for _, authorize := range []bool{false, true} {
		t.Run(map[bool]string{false: "CAS-refused", true: "granted-before-wake"}[authorize], func(t *testing.T) {
			dir, bin := t.TempDir(), resumeAgent(t)
			// A crash can happen after CAS and before the best-effort wake.
			// Removing only that wake leaves real channel acceptance/owner CAS.
			f := assembledResumeAt(t, dir, bin, nil, func(cb *turn.Callbacks) { cb.ResumeDispatcher = func(turn.TaskResume) {} })
			before := f.paused(t, "console")
			if !authorize {
				if _, err := f.book.DB().Exec(`CREATE TRIGGER refuse_task_resume BEFORE UPDATE ON bindings WHEN NEW.kind='task' BEGIN SELECT RAISE(ABORT, 'refused'); END`); err != nil {
					t.Fatal(err)
				}
			}
			req := turn.Request{Channel: "console", ConversationID: before.Channel, MessageID: "stable-control", Input: "/tasks resume " + before.ID}
			_, err := f.coordinator.Handle(t.Context(), req)
			if (err == nil) != authorize {
				t.Fatalf("resume grant=%v: %v", authorize, err)
			}
			queue := f.console.Queue(before.Channel)
			if len(queue) != 1 || queue[0].State != "queued" || !queue[0].ResumeAdmission.Valid() {
				t.Fatalf("resume was not durably dormant: %+v", queue)
			}
			original := queue[0]
			if err := f.console.Shutdown(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := f.book.Close(); err != nil {
				t.Fatal(err)
			}
			next := assembledResumeAt(t, dir, bin, nil)
			if err := next.console.Drain(); err != nil {
				t.Fatal(err)
			}
			if !authorize {
				current := next.console.Queue(before.Channel)
				if len(current) != 1 || current[0].ID != original.ID || current[0].State != "queued" || current[0].ResumeAdmission != original.ResumeAdmission {
					t.Fatalf("reopen promoted an ungranted input: %+v", current)
				}
				native, err := attempt.New(next.book).ForTask(t.Context(), before.ID)
				if err != nil || len(native) != 0 {
					t.Fatalf("ungranted reopen created native work: %+v, %v", native, err)
				}
				if _, err := next.book.DB().Exec(`DROP TRIGGER refuse_task_resume`); err != nil {
					t.Fatal(err)
				}
				// Same stable key can complete its original CAS without
				// replacing the previously accepted input.
				if _, err := next.coordinator.Handle(t.Context(), req); err != nil {
					t.Fatal(err)
				}
			}
			waitResumeReceipt(t, next, before.Channel, original.ID)
			after, _ := next.tasks.Get(before.ID)
			native, err := attempt.New(next.book).ForTask(t.Context(), before.ID)
			if err != nil || len(native) != 1 || native[0].TurnID != console.AnchorMark+original.ID || native[0].State != attempt.Bound ||
				after.Budget.Turns != before.Budget.Turns+1 || !after.ResumeGrant.Consumed || len(next.console.Queue(before.Channel)) != 1 {
				t.Fatalf("reopen did not consume original input once: task=%+v native=%+v err=%v", after, native, err)
			}
		})
	}
}

// Delay only consumption, after the real console has durably accepted input.
// No task owner, channel admission, or native execution is replaced by a fake.
type resumeInputGate struct {
	*turn.Coordinator
	entered chan turn.Request
	release chan struct{}
}

func (g *resumeInputGate) Handle(ctx context.Context, req turn.Request) (turn.Result, error) {
	if req.ExpectedTask != "" {
		select {
		case g.entered <- req:
		case <-ctx.Done():
			return turn.Result{}, ctx.Err()
		}
		select {
		case <-g.release:
		case <-ctx.Done():
			return turn.Result{}, ctx.Err()
		}
	}
	return g.Coordinator.Handle(ctx, req)
}

func (f resumeFixture) paused(t *testing.T, transport string) task.Task {
	t.Helper()
	tracked, err := f.tasks.Create(task.Task{
		Transport: transport, Channel: "console:resume", Member: "codex", ProjectID: "p",
		AnchorMessage: "original", Goal: "unfinished work",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.tasks.Begin(tracked.ID, "codex", "test", "native"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.tasks.Finish(tracked.ID, task.OutcomeOK, task.Tokens{Total: 17}, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := f.tasks.SetAside(tracked.ID, task.StatePaused); err != nil {
		t.Fatal(err)
	}
	tracked, _ = f.tasks.Get(tracked.ID)
	if err := f.sessions.SetActiveAgent(tracked.Channel, "codex"); err != nil {
		t.Fatal(err)
	}
	return tracked
}

func TestTaskResumeProductionRefusalLeavesTaskPaused(t *testing.T) {
	for _, scenario := range []string{"missing-transport", "unknown-transport", "feishu-unavailable", "console-closed", "console-write-failure", "revive-write-failure"} {
		t.Run(scenario, func(t *testing.T) {
			f := assembledResume(t)
			transport := "console"
			switch scenario {
			case "missing-transport":
				transport = ""
			case "unknown-transport":
				transport = "unknown"
			case "feishu-unavailable":
				transport = "feishu"
			}
			before := f.paused(t, transport)
			switch scenario {
			case "console-closed":
				if err := f.console.Shutdown(t.Context()); err != nil {
					t.Fatal(err)
				}
			case "console-write-failure":
				if _, err := f.book.DB().Exec(`CREATE TRIGGER refuse_resume_console BEFORE INSERT ON bindings
					WHEN NEW.kind='console-exchange'
					BEGIN SELECT RAISE(ABORT, 'console queue unavailable'); END`); err != nil {
					t.Fatal(err)
				}
			case "revive-write-failure":
				if err := f.sessions.SaveSession(state.Session{ConversationID: before.Channel, AgentID: "codex", Tainted: true}); err != nil {
					t.Fatal(err)
				}
				if _, err := f.book.DB().Exec(`CREATE TRIGGER refuse_resume_session BEFORE UPDATE ON bindings
					WHEN NEW.kind='document' AND NEW.id='state'
					BEGIN SELECT RAISE(ABORT, 'session revival unavailable'); END`); err != nil {
					t.Fatal(err)
				}
			}
			reopened, err := task.OpenLedger(f.book)
			if err != nil {
				t.Fatal(err)
			}
			durableBefore, _ := reopened.Get(before.ID)
			result, err := f.coordinator.Handle(t.Context(), turn.Request{
				Channel: transport, ConversationID: before.Channel, Locale: "en",
				MessageID: "control-refusal",
				Input:     "/tasks resume " + before.ID,
			})
			if err == nil || result.Text == "" || strings.Contains(result.Text, "is running again") {
				t.Errorf("refused resume reported success: result=%+v err=%v", result, err)
			}
			after, _ := f.tasks.Get(before.ID)
			if !reflect.DeepEqual(before, after) {
				t.Errorf("refused resume changed task state, epoch or accounting: before=%+v after=%+v", before, after)
			}
			reopened, err = task.OpenLedger(f.book)
			if err != nil {
				t.Fatal(err)
			}
			durableAfter, _ := reopened.Get(before.ID)
			if !reflect.DeepEqual(durableBefore, durableAfter) {
				t.Error("refused resume changed durable owner state")
			}
			if len(f.console.Queue(before.Channel)) != 0 {
				t.Error("refused resume retained a continuation")
			}
		})
	}
}

func TestTaskResumeProductionConsoleReentersOnlyAfterAdmission(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "mockagent")
	if output, err := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent").CombinedOutput(); err != nil {
		t.Fatalf("build isolated native test agent: %v\n%s", err, output)
	}
	for _, refuseTaskWrite := range []bool{false, true} {
		t.Run(map[bool]string{false: "accepted", true: "task-write-refused"}[refuseTaskWrite], func(t *testing.T) {
			f := assembledResumeWithHarness(t, bin)
			before := f.paused(t, "console")
			if refuseTaskWrite {
				// Cover the task owner's record layout and its preceding document
				// layout without depending on either one for assertions.
				if _, err := f.book.DB().Exec(`CREATE TRIGGER refuse_resume_task BEFORE UPDATE ON bindings
					WHEN NEW.kind='task' OR (NEW.kind='document' AND NEW.id='tasks')
					BEGIN SELECT RAISE(ABORT, 'task admission unavailable'); END`); err != nil {
					t.Fatal(err)
				}
			}
			_, err := f.coordinator.Handle(t.Context(), turn.Request{
				Channel: "console", ConversationID: before.Channel, Locale: "en", Input: "/tasks resume " + before.ID,
				MessageID: "control-resume",
			})
			if (err != nil) != refuseTaskWrite {
				t.Fatalf("resume write refusal=%v: %v", refuseTaskWrite, err)
			}
			if refuseTaskWrite {
				// Dormant input survives a failed task CAS, but neither Drain
				// nor another user's resume may turn acceptance into authority.
				if err := f.console.Drain(); err != nil {
					t.Fatal(err)
				}
				queue := f.console.Queue(before.Channel)
				if len(queue) != 1 || queue[0].ExpectedTask != before.ID || queue[0].State != "queued" {
					t.Fatalf("unauthorized input was not retained dormant: %+v", queue)
				}
				after, _ := f.tasks.Get(before.ID)
				if !reflect.DeepEqual(before, after) {
					t.Fatalf("failed task admission mutated task: before=%+v after=%+v", before, after)
				}
				native, err := attempt.New(f.book).ForTask(t.Context(), before.ID)
				if err != nil || len(native) != 0 {
					t.Fatalf("unauthorized input created native attempts: %+v, %v", native, err)
				}
				return
			}
			deadline := time.After(15 * time.Second)
			for {
				queue := f.console.Queue(before.Channel)
				if len(queue) != 1 || queue[0].ExpectedTask != before.ID {
					t.Fatalf("resume did not enqueue the original task: %+v", queue)
				}
				if queue[0].State.Terminal() {
					break
				}
				select {
				case <-deadline:
					t.Fatal("accepted resume did not release the turn slot")
				case <-time.After(5 * time.Millisecond):
				}
			}
			after, _ := f.tasks.Get(before.ID)
			if after.State != task.StateRunning || after.ExecutionEpoch != before.ExecutionEpoch+1 ||
				len(after.Attempts) != len(before.Attempts)+1 || after.Attempts[len(after.Attempts)-1].Outcome != task.OutcomeOK {
				t.Fatalf("accepted re-entry did not run exactly once against its original task: %+v", after)
			}
			if len(f.tasks.List(before.Channel)) != 1 {
				t.Fatal("resume opened a different task")
			}
			reopened, err := task.OpenLedger(f.book)
			if err != nil {
				t.Fatal(err)
			}
			durable, _ := reopened.Get(before.ID)
			if durable.ExecutionEpoch != after.ExecutionEpoch || len(durable.Attempts) != len(after.Attempts) || durable.Budget != after.Budget {
				t.Fatal("accepted native resume was not durable")
			}
		})
	}
}

func TestTaskResumeRejectedOrRevokedInputCannotBorrowLaterResume(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "mockagent")
	if output, err := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent").CombinedOutput(); err != nil {
		t.Fatalf("build isolated native test agent: %v\n%s", err, output)
	}
	for _, scenario := range []string{"task-write-refused", "pause-before-consumption"} {
		t.Run(scenario, func(t *testing.T) {
			gate := &resumeInputGate{entered: make(chan turn.Request, 4), release: make(chan struct{})}
			f := assembledResumeWithGate(t, bin, gate)
			t.Cleanup(func() { close(gate.release) })
			before := f.paused(t, "console")
			if scenario == "task-write-refused" {
				if _, err := f.book.DB().Exec(`CREATE TRIGGER refuse_resume_task BEFORE UPDATE ON bindings
					WHEN NEW.kind='task'
					BEGIN SELECT RAISE(ABORT, 'task admission unavailable'); END`); err != nil {
					t.Fatal(err)
				}
			}
			resumeCount := 0
			resume := func() error {
				resumeCount++
				_, err := f.coordinator.Handle(t.Context(), turn.Request{
					Channel: "console", ConversationID: before.Channel, Locale: "en", Input: "/tasks resume " + before.ID,
					MessageID: fmt.Sprintf("control-%d", resumeCount),
				})
				return err
			}
			if err := resume(); (err != nil) != (scenario == "task-write-refused") {
				t.Fatalf("first resume: %v", err)
			}
			var obsolete turn.Request
			if scenario == "task-write-refused" {
				if err := f.console.Drain(); err != nil {
					t.Fatal(err)
				}
				select {
				case <-gate.entered:
					t.Fatal("ungranted input started the real coordinator")
				default:
				}
				unchanged, _ := f.tasks.Get(before.ID)
				if !reflect.DeepEqual(before, unchanged) {
					t.Fatal("failed authorization already mutated the task")
				}
				if _, err := f.book.DB().Exec(`DROP TRIGGER refuse_resume_task`); err != nil {
					t.Fatal(err)
				}
			} else {
				select {
				case obsolete = <-gate.entered:
				case <-time.After(5 * time.Second):
					t.Fatal("first granted input did not reach the consumption gate")
				}
				if obsolete.ExpectedTask != before.ID {
					t.Fatal("fixture lost original task identity")
				}
				if _, err := f.coordinator.Handle(t.Context(), turn.Request{
					Channel: "console", ConversationID: before.Channel, Input: "/tasks pause " + before.ID,
				}); err != nil {
					t.Fatal(err)
				}
				stopped, _ := f.tasks.Get(before.ID)
				if stopped.State != task.StatePaused {
					t.Fatal("late pause did not revoke the first resume")
				}
			}
			if err := resume(); err != nil {
				t.Fatalf("new user resume: %v", err)
			}
			if scenario == "pause-before-consumption" {
				gate.release <- struct{}{}
			}
			// The next input is kept outside the coordinator while inspecting
			// what the obsolete one did. Its future success cannot mask a replay.
			select {
			case current := <-gate.entered:
				if current.ExchangeID == obsolete.ExchangeID {
					t.Fatal("new resume reused the obsolete input identity")
				}
			case <-time.After(15 * time.Second):
				t.Fatal("obsolete input did not finish and release the console queue")
			}
			afterObsolete, _ := f.tasks.Get(before.ID)
			if len(afterObsolete.Attempts) != len(before.Attempts) || afterObsolete.Budget != before.Budget {
				t.Errorf("obsolete input borrowed the later resume's authority: before=%+v after=%+v", before, afterObsolete)
			}
			native, err := attempt.New(f.book).ForTask(t.Context(), before.ID)
			if err != nil || len(native) != 0 {
				t.Errorf("obsolete input created native attempts: %+v, %v", native, err)
			}
			if len(f.tasks.List(before.Channel)) != 1 {
				t.Fatal("obsolete input retargeted to another task")
			}
		})
	}
}
