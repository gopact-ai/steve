package turn

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/task"
)

func noticeCoordinator(t *testing.T, runner *fakeRunner, after time.Duration) (*Coordinator, chan TaskNotice) {
	t.Helper()
	coordinator, _ := taskCoordinator(t, runner)
	notices := make(chan TaskNotice, 4)
	coordinator.SetNotifier(func(n TaskNotice) { notices <- n })
	coordinator.SetOfflineReminder(after)
	return coordinator, notices
}

// A long turn whose asker never came back has to be announced, not just
// rendered: the card is delivery to a chat, the ping is delivery to a person.
func TestOfflineReminderFiresWhenTheAskerWentQuiet(t *testing.T) {
	coordinator, notices := noticeCoordinator(t, &fakeRunner{reply: "ok"}, time.Nanosecond)

	if _, err := coordinator.Handle(t.Context(), Request{
		ConversationID: "chat", Input: "crunch the archive",
		MessageID: "om_anchor", ChatID: "oc_chat", SenderOpenID: "ou_asker",
	}); err != nil {
		t.Fatalf("turn: %v", err)
	}

	select {
	case got := <-notices:
		if got.TaskID != "1" || got.MessageID != "om_anchor" || got.Requester != "ou_asker" {
			t.Fatalf("notice = %+v; want task 1 anchored at om_anchor for the asker", got)
		}
		if !strings.Contains(got.Text, "#1") {
			t.Fatalf("notice text = %q; want it to name the task", got.Text)
		}
	default:
		t.Fatal("no reminder for a long turn nobody was watching")
	}
}

// Somebody who is still typing does not need to be told their answer arrived.
func TestOfflineReminderStaysQuietWhenTheAskerIsPresent(t *testing.T) {
	runner := &fakeRunner{reply: "ok", started: make(chan struct{}), done: make(chan struct{})}
	coordinator, notices := noticeCoordinator(t, runner, time.Nanosecond)

	finished := make(chan error, 1)
	go func() {
		_, err := handle(coordinator, t.Context(), "a long job")
		finished <- err
	}()
	<-runner.started
	// A message arriving mid-turn is the evidence that the asker is here.
	coordinator.noteActivity("chat")
	close(runner.done)
	if err := <-finished; err != nil {
		t.Fatalf("turn: %v", err)
	}

	select {
	case got := <-notices:
		t.Fatalf("pinged someone who was still in the conversation: %+v", got)
	default:
	}
}

func TestOfflineReminderStaysQuietForShortAndFailedTurns(t *testing.T) {
	coordinator, notices := noticeCoordinator(t, &fakeRunner{reply: "ok"}, time.Hour)
	if _, err := handle(coordinator, t.Context(), "quick one"); err != nil {
		t.Fatalf("turn: %v", err)
	}
	select {
	case got := <-notices:
		t.Fatalf("short turn pinged: %+v", got)
	default:
	}

	failing, failedNotices := noticeCoordinator(t, &fakeRunner{err: errors.New("boom")}, time.Nanosecond)
	if _, err := handle(failing, t.Context(), "doomed"); err == nil {
		t.Fatal("expected the turn to fail")
	}
	select {
	case got := <-failedNotices:
		// The failure card already says so and @s the asker; repeating it
		// in plain text is noise, not delivery.
		t.Fatalf("failed turn pinged: %+v", got)
	default:
	}
}

// The brake has to say where the work got to. "Budget spent" alone leaves the
// user unable to tell a stop from a loss.
func TestBudgetStopSaysWhereItGotTo(t *testing.T) {
	coordinator, tasks := taskCoordinator(t, &fakeRunner{reply: "ok"})

	for i := 0; i <= task.DefaultMaxTurns; i++ {
		_, err := handle(coordinator, t.Context(), "again")
		if err == nil {
			continue
		}
		var userErr UserError
		if !errors.As(err, &userErr) {
			t.Fatalf("budget stop must be a UserError, got %T: %v", err, err)
		}
		for _, want := range []string{"跑到哪了", "尝试", "ok", "again"} {
			if !strings.Contains(userErr.Text, want) {
				t.Fatalf("budget stop = %q; want it to contain %q", userErr.Text, want)
			}
		}
		if tracked := tasks.List("chat")[0]; !strings.Contains(userErr.Text, tracked.ID) {
			t.Fatalf("budget stop should name the task: %q", userErr.Text)
		}
		return
	}
	t.Fatal("the turn budget never stopped the task")
}
