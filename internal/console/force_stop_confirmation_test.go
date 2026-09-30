package console

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/turn"
)

type forceConfirmationCapture struct {
	calls   []string
	failure error
}

func (f *forceConfirmationCapture) ForceStopAttempt(_ context.Context, id, _ string) error {
	f.calls = append(f.calls, id)
	return f.failure
}

func TestForceConfirmationKeepsItsTargetAndRequiresAnExplicitChoice(t *testing.T) {
	for _, mode := range []string{"cancel", "confirm", "failure", "restart"} {
		t.Run(mode, func(t *testing.T) {
			s := New(&echo{}, "owner", nil)
			doc := &memDoc{}
			if err := s.Persist(doc); err != nil {
				t.Fatal(err)
			}
			driver := &recoveryDriver{}
			s.recoveryDriver = driver
			force := &forceConfirmationCapture{}
			if mode == "failure" {
				force.failure = errors.New("original execution is unavailable")
			}
			s.SetForceStops(force)
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			w := &recoveryStopWait{s: s, e: &queuedExchange{Exchange: Exchange{ID: "e1", Conversation: "console:main", Requester: "owner"}}, base: consoleapi.PendingQuestion{Conversation: "console:main", ExchangeID: "e1", Locale: "en"}, ctx: ctx, requester: "owner"}
			result := make(chan error, 1)
			go func() { result <- w.forceStop() }()
			q := awaitOffer(t, s, "confirm-force-stop")
			if q.TaskID != "task-1" || q.AttemptID != "attempt-1" || q.AllowFreeText {
				t.Fatalf("confirmation=%+v", q)
			}
			if _, err := s.AnswerQuestion(t.Context(), q.ID, consoleapi.QuestionAnswer{CommandID: "free-text", Decision: "accept", Text: "yes"}); !errors.Is(err, consoleapi.ErrInvalidAnswer) {
				t.Fatal("free text bypassed explicit confirmation")
			}
			// The next observation could name another execution; confirmation
			// must use the identity actually presented, not perform another lookup.
			driver.candidates = []turn.RetainedChat{{AttemptID: "new-attempt", TaskID: "new-task", Conversation: "console:main", MessageID: "web-e1"}}
			if mode == "restart" {
				restored := New(&echo{}, "owner", nil)
				if err := restored.Persist(doc); err != nil {
					t.Fatal(err)
				}
				if _, err := restored.AnswerQuestion(t.Context(), q.ID, consoleapi.QuestionAnswer{CommandID: "stale-confirm", Decision: "accept", Choice: "confirm-force-stop"}); err == nil {
					t.Fatal("orphaned confirmation remained actionable")
				}
				cancel()
				<-result
			} else {
				choice := "confirm-force-stop"
				if mode == "cancel" {
					choice = "cancel-force-stop"
				}
				answer := consoleapi.QuestionAnswer{CommandID: "confirmed-original", Decision: "accept", Choice: choice}
				if _, err := s.AnswerQuestion(t.Context(), q.ID, answer); err != nil {
					t.Fatal(err)
				}
				if _, err := s.AnswerQuestion(t.Context(), q.ID, answer); err != nil {
					t.Fatal(err)
				}
				err := <-result
				if !errors.Is(err, force.failure) {
					t.Fatalf("confirmation outcome=%v", err)
				}
			}
			want := 0
			if mode == "confirm" || mode == "failure" {
				want = 1
			}
			if len(force.calls) != want || want == 1 && force.calls[0] != "attempt-1" {
				t.Fatalf("calls=%v want original once=%d", force.calls, want)
			}
		})
	}
}
