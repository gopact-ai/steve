package turn

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/task"
)

func tasksCmdText(t *testing.T, c *Coordinator, input string) string {
	t.Helper()
	result, err := handle(c, t.Context(), input)
	if err != nil {
		t.Fatalf("%s: %v", input, err)
	}
	return result.Text
}

// Pausing is the user setting work down, and it has to do both halves: stop
// the turn that is burning time now, and let the next message start something
// new instead of charging the task that was set aside.
func TestPauseStopsTheRunningTurnAndFreesTheConversation(t *testing.T) {
	runner := &fakeRunner{reply: "ok", started: make(chan struct{}), done: make(chan struct{})}
	coordinator, tasks := taskCoordinator(t, runner)

	first := make(chan error, 1)
	go func() {
		_, err := handle(coordinator, t.Context(), "a long job")
		first <- err
	}()
	<-runner.started

	text := tasksCmdText(t, coordinator, "/tasks pause")
	if !strings.Contains(text, "#1") || !strings.Contains(text, "paused") {
		t.Fatalf("pause card = %q; want it to name task #1 and its new state", text)
	}
	select {
	case err := <-first:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("paused turn ended with %v; want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pause did not stop the running turn")
	}
	if tracked, _ := tasks.Get("1"); tracked.State != task.StatePaused {
		t.Fatalf("state = %q; want paused", tracked.State)
	}

	// The conversation is free again: the next message opens its own task
	// rather than quietly reviving the one the user set aside.
	runner.canceled.Store(false)
	if _, err := handle(coordinator, t.Context(), "something else now"); err != nil {
		t.Fatalf("follow-up turn: %v", err)
	}
	all := tasks.List("chat")
	if len(all) != 2 {
		t.Fatalf("tasks = %d; want the paused one plus a new one", len(all))
	}
	if paused, _ := tasks.Get("1"); paused.State != task.StatePaused {
		t.Fatalf("paused task moved to %q under a new message", paused.State)
	}
}

// Resume hands the channel everything it needs to replay the task: without a
// fresh anchor message the resumed turn would have nothing to render against.
func TestResumeReplaysTheTaskThroughTheChannel(t *testing.T) {
	coordinator, tasks := taskCoordinator(t, &fakeRunner{reply: "ok"})
	if _, err := coordinator.Handle(t.Context(), Request{
		ConversationID: "chat", Input: "index the archive",
		MessageID: "om_anchor", ChatID: "oc_chat", SenderOpenID: "ou_asker",
	}); err != nil {
		t.Fatalf("opening turn: %v", err)
	}
	if _, err := tasks.Advance("1", task.StatePaused); err != nil {
		t.Fatalf("pause: %v", err)
	}

	resumed := make(chan TaskResume, 1)
	coordinator.SetResumer(func(r TaskResume) { resumed <- r })

	tasksCmdText(t, coordinator, "/tasks resume")
	select {
	case got := <-resumed:
		if got.TaskID != "1" || got.MessageID != "om_anchor" || got.Member != "codex" {
			t.Fatalf("resume request = %+v; want task 1 anchored at om_anchor on codex", got)
		}
		if got.Goal != "index the archive" || got.Requester != "ou_asker" {
			t.Fatalf("resume request lost the goal or requester: %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("resume never reached the channel")
	}
	// The state moves before the replay, so the replayed message finds this
	// task instead of opening a second one for the same work.
	if tracked, _ := tasks.Get("1"); tracked.State != task.StateRunning {
		t.Fatalf("state = %q; want running before the replay lands", tracked.State)
	}
}

// A cancelled task is terminal: nothing resumes it, and a restart must not
// revive it behind the user's back.
func TestCancelIsTerminalAndSurvivesRestartUnrevived(t *testing.T) {
	coordinator, tasks := taskCoordinator(t, &fakeRunner{reply: "ok"})
	if _, err := handle(coordinator, t.Context(), "chase the flake"); err != nil {
		t.Fatalf("opening turn: %v", err)
	}

	text := tasksCmdText(t, coordinator, "/tasks cancel 1")
	if !strings.Contains(text, "#1") {
		t.Fatalf("cancel card = %q; want it to name the task", text)
	}
	tracked, _ := tasks.Get("1")
	if tracked.State != task.StateCancelled || !tracked.State.Terminal() {
		t.Fatalf("state = %q terminal=%t; want a terminal cancelled", tracked.State, tracked.State.Terminal())
	}
	for _, interrupted := range tasks.Interrupted() {
		if interrupted.ID == "1" {
			t.Fatal("a cancelled task must not be offered for revival")
		}
	}
	if _, err := handle(coordinator, t.Context(), "different work"); err != nil {
		t.Fatalf("follow-up turn: %v", err)
	}
	if len(tasks.List("chat")) != 2 {
		t.Fatal("a cancelled task must not keep claiming the conversation")
	}
}

// The detail view exists to answer "where did it get to" — which means the
// attempts it survived, not just the ones that worked.
func TestTaskDetailShowsTheAttemptTrail(t *testing.T) {
	coordinator, tasks := taskCoordinator(t, &fakeRunner{reply: "ok"})
	tasks.SetBudget(24, time.Hour)
	if _, err := handle(coordinator, t.Context(), "port the parser"); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	if _, err := tasks.Begin("1", "codex", "laptop", ""); err != nil {
		t.Fatalf("second attempt: %v", err)
	}
	if _, err := tasks.Finish("1", task.OutcomeInterrupted, task.Tokens{}, 0); err != nil {
		t.Fatalf("interrupt the second attempt: %v", err)
	}

	text := tasksCmdText(t, coordinator, "/tasks 1")
	for _, want := range []string{"#1", "port the parser", "ok", "interrupted", "2/"} {
		if !strings.Contains(text, want) {
			t.Fatalf("detail = %q; want it to contain %q", text, want)
		}
	}
}

// Task ids are short and guessable. Reaching one from another conversation
// would let any chat stop work it cannot even see.
func TestTaskCommandsCannotReachAnotherConversation(t *testing.T) {
	coordinator, tasks := taskCoordinator(t, &fakeRunner{reply: "ok"})
	if _, err := handle(coordinator, t.Context(), "private work"); err != nil {
		t.Fatalf("opening turn: %v", err)
	}

	result, err := coordinator.Handle(t.Context(), Request{ConversationID: "other", Input: "/tasks cancel 1"})
	if err != nil {
		t.Fatalf("cross-conversation cancel: %v", err)
	}
	if !strings.Contains(result.Text, "1") {
		t.Fatalf("reply = %q; want it to say the task is not here", result.Text)
	}
	if tracked, _ := tasks.Get("1"); tracked.State == task.StateCancelled {
		t.Fatal("another conversation cancelled a task it cannot see")
	}
}

func TestParseTaskArgs(t *testing.T) {
	for _, tc := range []struct {
		in   string
		verb taskVerb
		id   string
		ok   bool
	}{
		{"", taskList, "", true},
		{"12", taskShow, "12", true},
		{"#12", taskShow, "12", true},
		{"pause", taskPause, "", true},
		{"pause 12", taskPause, "12", true},
		{"12 pause", taskPause, "12", true},
		{"暂停 3", taskPause, "3", true},
		{"继续", taskResume, "", true},
		{"取消 #7", taskCancel, "7", true},
		{"CANCEL 7", taskCancel, "7", true},
		{"halt 7", taskList, "", false},
	} {
		verb, id, ok := parseTaskArgs(tc.in)
		if verb != tc.verb || id != tc.id || ok != tc.ok {
			t.Errorf("parseTaskArgs(%q) = %v,%q,%t; want %v,%q,%t", tc.in, verb, id, ok, tc.verb, tc.id, tc.ok)
		}
	}
}
