package console

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/turn"
)

func TestAbandonRecoveryConfirmationKeepsTheOriginalAttemptAndRevision(t *testing.T) {
	for _, mode := range []string{"confirm", "cancel", "failure"} {
		t.Run(mode, func(t *testing.T) {
			s := New(&echo{}, "owner", nil)
			if err := s.Persist(&memDoc{}); err != nil {
				t.Fatal(err)
			}
			driver := &recoveryDriver{candidates: []turn.RetainedChat{{AttemptID: "original", TaskID: "task-1", Conversation: "console:main", MessageID: "web-e1", ForceStopRevision: 7, ForceStopLevel: "exhausted"}}}
			s.recoveryDriver = driver
			control := &abandonDriverFixture{record: attempt.Record{Spec: attempt.Spec{ID: "original"}, Abandoned: &attempt.Abandoned{At: time.Now(), By: "owner", ForceStopRevision: 7}}}
			if mode == "failure" {
				control.error = errors.New("confirmation changed")
			}
			s.SetAbandons(control)
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			w := &recoveryStopWait{s: s, e: &queuedExchange{Exchange: Exchange{ID: "e1", Conversation: "console:main", Requester: "owner"}}, ctx: ctx, requester: "owner", base: consoleapi.PendingQuestion{Conversation: "console:main", ExchangeID: "e1", Locale: "en"}}
			done := make(chan error, 1)
			go func() { done <- w.abandon() }()
			q := awaitOffer(t, s, "confirm-abandon")
			if q.TaskID != "task-1" || q.AttemptID != "original" || q.AllowFreeText {
				t.Fatalf("question lost original identity: %+v", q)
			}
			if _, err := s.AnswerQuestion(t.Context(), q.ID, consoleapi.QuestionAnswer{CommandID: "text", Decision: "accept", Text: "yes"}); !errors.Is(err, consoleapi.ErrInvalidAnswer) {
				t.Fatal("free text bypassed abandonment confirmation")
			}
			driver.candidates = []turn.RetainedChat{{AttemptID: "newer", TaskID: "task-2", Conversation: "console:main", MessageID: "web-e1", ForceStopRevision: 8, ForceStopLevel: "exhausted"}}
			choice := "confirm-abandon"
			if mode == "cancel" {
				choice = "cancel-abandon"
			}
			answer := consoleapi.QuestionAnswer{CommandID: "explicit", Decision: "accept", Choice: choice}
			if _, err := s.AnswerQuestion(t.Context(), q.ID, answer); err != nil {
				t.Fatal(err)
			}
			if _, err := s.AnswerQuestion(t.Context(), q.ID, answer); err != nil {
				t.Fatal(err)
			}
			if err := <-done; !errors.Is(err, control.error) {
				t.Fatalf("result=%v", err)
			}
			want := 1
			if mode == "cancel" {
				want = 0
			}
			if control.calls != want || want == 1 && (control.id != "original" || control.revision != 7) {
				t.Fatalf("abandonment retargeted or repeated: %+v", control)
			}
		})
	}
}
