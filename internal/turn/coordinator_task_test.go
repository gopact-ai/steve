package turn

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
)

func taskCoordinator(t *testing.T, runner *fakeRunner) (*Coordinator, *task.Store) {
	t.Helper()
	catalog, _ := agent.NewCatalog(map[string]agent.Config{
		"codex": {Harness: "codex", Default: true},
	})
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	tasks, err := task.Open(filepath.Join(t.TempDir(), "tasks.json"))
	if err != nil {
		t.Fatalf("open tasks: %v", err)
	}
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": runner}}
	coordinator := newCoordinator(t, catalog, store, capability.NewAssembler(nil), manager, time.Minute)
	coordinator.SetTasks(tasks, "laptop")
	return coordinator, tasks
}

func TestTurnOpensOneTaskAndKeepsUsingIt(t *testing.T) {
	coordinator, tasks := taskCoordinator(t, &fakeRunner{reply: "ok"})

	if _, err := handle(coordinator, t.Context(), "wire the node link"); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	if _, err := handle(coordinator, t.Context(), "now add reconnect"); err != nil {
		t.Fatalf("second turn: %v", err)
	}

	all := tasks.List("chat")
	if len(all) != 1 {
		t.Fatalf("tasks = %d; want one task spanning both turns", len(all))
	}
	tracked := all[0]
	if tracked.Goal != "wire the node link" {
		t.Fatalf("goal = %q; want the opening prompt", tracked.Goal)
	}
	if tracked.Budget.Turns != 2 || len(tracked.Attempts) != 2 {
		t.Fatalf("turns=%d attempts=%d; want 2 and 2", tracked.Budget.Turns, len(tracked.Attempts))
	}
	if tracked.Member != "codex" || tracked.Node != "laptop" {
		t.Fatalf("member=%q node=%q", tracked.Member, tracked.Node)
	}
	for i, attempt := range tracked.Attempts {
		if attempt.Open() || attempt.Outcome != task.OutcomeOK {
			t.Fatalf("attempt %d not closed cleanly: %+v", i, attempt)
		}
	}
}

func TestFailedTurnStillRecordsTheAttempt(t *testing.T) {
	coordinator, tasks := taskCoordinator(t, &fakeRunner{err: errors.New("prompt failed")})

	if _, err := handle(coordinator, t.Context(), "do the thing"); err == nil {
		t.Fatal("expected the turn to fail")
	}
	all := tasks.List("chat")
	if len(all) != 1 || len(all[0].Attempts) != 1 {
		t.Fatalf("tasks = %+v; want one task with one attempt", all)
	}
	if got := all[0].Attempts[0].Outcome; got != task.OutcomeError {
		t.Fatalf("outcome = %q; want error", got)
	}
}

func TestBudgetStopsTheTaskAndNamesTheLimit(t *testing.T) {
	coordinator, tasks := taskCoordinator(t, &fakeRunner{reply: "ok"})

	// Drive turns until the default cap trips. The brake lives in the task
	// store, so every caller gets it without having to remember to check.
	for i := 0; i <= task.DefaultMaxTurns; i++ {
		_, err := handle(coordinator, t.Context(), "again")
		if err == nil {
			continue
		}
		var userErr UserError
		if !errors.As(err, &userErr) {
			t.Fatalf("budget stop must be a UserError, got %T: %v", err, err)
		}
		tracked := tasks.List("chat")[0]
		if !strings.Contains(userErr.Text, tracked.ID) {
			t.Fatalf("message should name the task: %q", userErr.Text)
		}
		if !strings.Contains(userErr.Text, "轮") {
			t.Fatalf("message should name the turn limit: %q", userErr.Text)
		}
		if tracked.Budget.Turns != task.DefaultMaxTurns {
			t.Fatalf("turns = %d; want the cap %d", tracked.Budget.Turns, task.DefaultMaxTurns)
		}
		return
	}
	t.Fatal("the turn budget never stopped the task")
}

// A "!"-prefixed message replaces the running turn instead of queueing
// behind it. Queueing is the default; the bang is the explicit "no, do it
// this way instead" — and the interrupted turn is still charged.
func TestBangMessageInterruptsTheRunningTurn(t *testing.T) {
	runner := &fakeRunner{reply: "ok", started: make(chan struct{}), done: make(chan struct{})}
	coordinator, tasks := taskCoordinator(t, runner)

	first := make(chan error, 1)
	go func() {
		_, err := handle(coordinator, t.Context(), "long running")
		first <- err
	}()
	<-runner.started

	second := make(chan error, 1)
	go func() {
		_, err := handle(coordinator, t.Context(), "! actually do this instead")
		second <- err
	}()

	// The first turn must come back cancelled without anyone closing
	// runner.done: the interrupt itself is what ends it.
	select {
	case err := <-first:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("interrupted turn ended with %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("interrupted turn never returned")
	}

	close(runner.done)
	select {
	case err := <-second:
		// The interrupting turn must run, not be told the agent is busy.
		if err != nil {
			t.Fatalf("interrupting turn failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("interrupting turn never returned")
	}
	if got := runner.seen(); len(got) != 2 || got[1] == got[0] || strings.Contains(got[1], "!") {
		t.Fatalf("agent saw %v, want both prompts with the bang stripped", got)
	}

	// Both turns reached the agent, so both are charged. An interrupted turn
	// did real work; pretending otherwise would let a user spin the budget
	// for free by interrupting forever.
	deadline := time.Now().Add(2 * time.Second)
	for {
		all := tasks.List("chat")
		if len(all) == 1 && all[0].Budget.Turns == 2 {
			open := 0
			for _, attempt := range all[0].Attempts {
				if attempt.Open() {
					open++
				}
			}
			if open == 0 {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("budget after interrupt: %+v", all)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestResetClosesTheTaskSoTheNextMessageStartsAFreshOne(t *testing.T) {
	coordinator, tasks := taskCoordinator(t, &fakeRunner{reply: "ok"})

	if _, err := handle(coordinator, t.Context(), "first goal"); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	if _, err := handle(coordinator, t.Context(), "/new"); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if _, err := handle(coordinator, t.Context(), "second goal"); err != nil {
		t.Fatalf("turn after reset: %v", err)
	}

	all := tasks.List("chat")
	if len(all) != 2 {
		t.Fatalf("tasks = %d; want a new task after /new", len(all))
	}
	var closed, open int
	for _, tracked := range all {
		if tracked.State.Terminal() {
			closed++
		} else {
			open++
		}
	}
	if closed != 1 || open != 1 {
		t.Fatalf("closed=%d open=%d; want exactly one of each", closed, open)
	}
}

func TestTaskTrackingIsOptional(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{
		"codex": {Harness: "codex", Default: true},
	})
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	manager := &fakeManager{runners: map[string]*fakeRunner{"codex": {reply: "ok"}}}
	coordinator := newCoordinator(t, catalog, store, capability.NewAssembler(nil), manager, time.Minute)

	if _, err := handle(coordinator, t.Context(), "no task store configured"); err != nil {
		t.Fatalf("turn without task tracking: %v", err)
	}
}

func TestTasksCommandListsWhatTheConversationDid(t *testing.T) {
	coordinator, _ := taskCoordinator(t, &fakeRunner{reply: "ok"})

	empty, err := handle(coordinator, t.Context(), "/tasks")
	if err != nil {
		t.Fatalf("empty listing: %v", err)
	}
	if !strings.Contains(empty.Text, "还没有任务") {
		t.Fatalf("empty listing = %q", empty.Text)
	}

	if _, err := handle(coordinator, t.Context(), "wire the node link"); err != nil {
		t.Fatalf("turn: %v", err)
	}
	listed, err := handle(coordinator, t.Context(), "/tasks")
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	for _, want := range []string{"#1", "wire the node link", "codex", "1/" + strconv.Itoa(task.DefaultMaxTurns)} {
		if !strings.Contains(listed.Text, want) {
			t.Fatalf("listing %q missing %q", listed.Text, want)
		}
	}
	// The listing itself must not charge a turn.
	if !strings.Contains(listed.Text, "1/"+strconv.Itoa(task.DefaultMaxTurns)) {
		t.Fatalf("listing spent a turn: %q", listed.Text)
	}
}

func TestStatusShowsTheLiveBudget(t *testing.T) {
	coordinator, _ := taskCoordinator(t, &fakeRunner{reply: "ok"})
	if _, err := handle(coordinator, t.Context(), "some goal"); err != nil {
		t.Fatalf("turn: %v", err)
	}
	result, err := handle(coordinator, t.Context(), "/status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	labels := map[string]string{}
	for _, f := range result.Fields {
		labels[f.Label] = f.Value
	}
	if labels["Task"] != "#1" {
		t.Fatalf("status fields = %+v; want Task #1", result.Fields)
	}
	if labels["Turns"] != "1/"+strconv.Itoa(task.DefaultMaxTurns) {
		t.Fatalf("Turns = %q", labels["Turns"])
	}
	if labels["Goal"] != "some goal" {
		t.Fatalf("Goal = %q", labels["Goal"])
	}
}

func TestStatusWithoutTaskTrackingHasNoTaskFields(t *testing.T) {
	catalog, _ := agent.NewCatalog(map[string]agent.Config{
		"codex": {Harness: "codex", Default: true},
	})
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	coordinator := newCoordinator(t, catalog, store, capability.NewAssembler(nil), &fakeManager{}, time.Minute)
	result, err := handle(coordinator, t.Context(), "/status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	for _, f := range result.Fields {
		if f.Label == "Task" || f.Label == "Turns" {
			t.Fatalf("task field leaked without a task store: %+v", result.Fields)
		}
	}
}
