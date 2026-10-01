package console

import (
	"fmt"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

func abandonDeliveryFixture(t *testing.T, kind attempt.Kind) (*Service, attempt.Record, *queuedExchange, *queuedExchange) {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = book.Close() })
	s := New(&echo{}, "owner", nil)
	if err := s.PersistLedger(book); err != nil {
		t.Fatal(err)
	}
	tasks, err := task.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	origin := "chat"
	if kind == attempt.KindPlan {
		origin = "plan"
	}
	tracked, err := tasks.Create(task.Task{Origin: origin, Transport: "console", Channel: "console:original", AnchorMessage: "web-original", ProjectID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	old := &queuedExchange{Exchange: Exchange{ID: "original", Conversation: tracked.Channel, State: consoleapi.ExchangeAwaitingUser, ExpectedProject: "p"}, done: make(chan struct{}), RecoveryStopPending: "unproved"}
	newer := &queuedExchange{Exchange: Exchange{ID: "newer", Conversation: tracked.Channel, State: consoleapi.ExchangeAwaitingUser, ExpectedProject: "p"}, done: make(chan struct{}), RecoveryStopPending: "unproved"}
	s.mu.Lock()
	s.exchanges[tracked.Channel] = []*queuedExchange{old, newer}
	s.running[tracked.Channel] = 2
	s.mu.Unlock()
	turnID := "web-original"
	if kind == attempt.KindPlan {
		turnID = fmt.Sprintf("plan/%s/r1/prompt/1", tracked.ID)
	}
	r := attempt.Record{Spec: attempt.Spec{ID: "original-attempt", TaskID: tracked.ID, Project: "p", Kind: kind, TurnID: turnID, Execution: &task.ExecutionToken{TaskID: tracked.ID, Epoch: 1}}, Abandoned: &attempt.Abandoned{At: time.Now(), ProjectedAt: time.Now(), Conversation: tracked.Channel, MessageID: "web-original"}}
	return s, r, old, newer
}
func TestAbandonmentDeliveryChecksItsImmutableInput(t *testing.T) {
	for _, kind := range []attempt.Kind{attempt.KindChat, attempt.KindPlan} {
		t.Run(string(kind), func(t *testing.T) {
			s, r, old, newer := abandonDeliveryFixture(t, kind)
			r.Abandoned.MessageID = "web-newer"
			if err := s.deliverAbandonment(t.Context(), r); err == nil {
				t.Fatal("accepted a receipt pointing to another input")
			}
			if newer.State.Terminal() || old.State.Terminal() {
				t.Fatal("mismatched receipt ended an exchange")
			}
			r.Abandoned.MessageID = "web-original"
			if err := s.deliverAbandonment(t.Context(), r); err != nil {
				t.Fatal(err)
			}
			if old.State != consoleapi.ExchangeCancelled || newer.State.Terminal() {
				t.Fatal("original abandonment ended another input")
			}
		})
	}
}
func TestAbandonmentOfATerminalInputDoesNotMoveToTheNextInput(t *testing.T) {
	for _, kind := range []attempt.Kind{attempt.KindChat, attempt.KindPlan} {
		t.Run(string(kind), func(t *testing.T) {
			s, r, old, newer := abandonDeliveryFixture(t, kind)
			old.State = consoleapi.ExchangeDone
			if err := s.deliverAbandonment(t.Context(), r); err != nil {
				t.Fatal(err)
			}
			if old.RecoveryAbandon != nil || newer.RecoveryAbandon != nil || newer.State.Terminal() {
				t.Fatal("a historical abandonment was reassigned")
			}
		})
	}
}
