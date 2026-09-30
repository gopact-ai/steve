package console

import (
	"context"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/task"
)

type forceStopCapture struct {
	calls chan string
	owner string
}

func (f *forceStopCapture) ForceStopAttempt(_ context.Context, id, owner string) error {
	f.owner = owner
	f.calls <- id
	return nil
}

func TestStopWaitForceChoiceTargetsTheOriginalAttempt(t *testing.T) {
	s := impatient(New(&echo{}, "owner", nil))
	s.EnableRetainedRecovery(t.Context())
	if err := s.Persist(recoveryDocument()); err != nil {
		t.Fatal(err)
	}
	driver := newCancelledTaskDriver()
	driver.setAside(task.StatePaused, "task-1")
	force := &forceStopCapture{calls: make(chan string, 1)}
	s.SetForceStops(force)
	if err := s.RecoverChats(t.Context(), driver); err != nil {
		t.Fatal(err)
	}
	card := awaitOffer(t, s, "force-stop")
	items, err := driver.RetainedChatsFor(t.Context(), "console:main", AnchorMark+"e1")
	if err != nil || len(items) != 1 {
		t.Fatalf("retained=%+v %v", items, err)
	}
	if _, err := s.AnswerQuestion(t.Context(), card.ID, consoleapi.QuestionAnswer{CommandID: "force-original", Decision: "accept", Choice: "force-stop"}); err != nil {
		t.Fatal(err)
	}
	confirm := awaitOffer(t, s, "confirm-force-stop")
	if confirm.ID == card.ID || confirm.TaskID != items[0].TaskID || confirm.AttemptID != items[0].AttemptID || confirm.AllowFreeText {
		t.Fatalf("confirmation lost original identity: %+v", confirm)
	}
	select {
	case <-force.calls:
		t.Fatal("force request sent before explicit confirmation")
	default:
	}
	if _, err := s.AnswerQuestion(t.Context(), confirm.ID, consoleapi.QuestionAnswer{CommandID: "confirm-original", Decision: "accept", Choice: "confirm-force-stop"}); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-force.calls:
		if id != items[0].AttemptID || force.owner != "owner" {
			t.Fatalf("force target=%s owner=%s", id, force.owner)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("force choice did not reach original attempt")
	}
	driver.confirmed.Store(true)
	answerRecheck(t, s, awaitStopWait(t, s))
	awaitExchange(t, s, "e1")
	awaitExchange(t, s, "e2")
}
