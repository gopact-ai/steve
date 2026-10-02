package console

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/ledger"
)

func TestAbandonmentAckFailureDoesNotAppendTheReplyAgain(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(map[bool]string{false: "same process", true: "reopen"}[restart], func(t *testing.T) {
			s, book, d, r := durableAbandonFixture(t)
			abandonDeliverySQL(t, book, `CREATE TRIGGER refuse_abandon_ack BEFORE UPDATE ON operations WHEN NEW.id='abandon-delivery' AND json_extract(NEW.data,'$.abandoned.delivery_done_at') IS NOT NULL BEGIN SELECT RAISE(ABORT,'ack refused'); END`)
			out, err := s.Abandon(t.Context(), r.ID, 1)
			if err != nil || !out.Accepted || !out.Pending {
				t.Fatalf("ack failure did not stay pending: %+v %v", out, err)
			}
			replies := s.Replies("delivery")
			if len(replies) != 1 {
				t.Fatalf("reply did not commit before ack: %+v", replies)
			}
			id := replies[0].ID
			current, _ := d.attempts.Get(t.Context(), r.ID)
			if !current.Abandoned.DeliveryDoneAt.IsZero() {
				t.Fatal("rejected ack was recorded")
			}
			abandonDeliverySQL(t, book, `DROP TRIGGER refuse_abandon_ack`)
			if restart {
				s, book = reopenAbandonReceiver(t, book, d)
			}
			for range 2 {
				if err := s.ReconcileAbandonments(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			replies = s.Replies("delivery")
			if len(replies) != 1 || replies[0].ID != id {
				t.Fatalf("ack retry repeated reply: %+v", replies)
			}
			current, _ = d.attempts.Get(t.Context(), r.ID)
			if current.Abandoned.DeliveryResult != attempt.AbandonDelivered || current.Abandoned.DeliveryDoneAt.IsZero() {
				t.Fatal("ack did not converge")
			}
		})
	}
}

func TestAbandonmentRecordsWhyNoNewReplyIsRequired(t *testing.T) {
	for _, mode := range []string{"child", "other channel", "gone", "already terminal"} {
		t.Run(mode, func(t *testing.T) {
			kind, conversation := attempt.KindChat, "console:delivery"
			if mode == "child" {
				kind = attempt.KindDelegate
			}
			if mode == "other channel" {
				conversation = "chat:other"
			}
			s, _, d, r := durableAbandonFixtureAt(t, kind, conversation)
			want := attempt.AbandonDeliveryNotRequired
			if mode == "gone" {
				if err := s.Discard(conversation); err != nil {
					t.Fatal(err)
				}
				want = attempt.AbandonDeliveryGone
			}
			if mode == "already terminal" {
				s.mu.Lock()
				target := s.exchanges[conversation][0]
				target.RecoveryStopPending = ""
				s.mu.Unlock()
				s.finish(target, consoleapi.Reply{Text: "the original answer", AttemptID: r.ID}, nil)
				want = attempt.AbandonDeliveryTerminal
			}
			before := s.Replies(conversation)
			out, err := s.Abandon(t.Context(), r.ID, 1)
			if err != nil || out.Pending {
				t.Fatalf("no-reply result=%+v %v", out, err)
			}
			current, _ := d.attempts.Get(t.Context(), r.ID)
			if current.Abandoned.DeliveryResult != want || current.Abandoned.DeliveryDoneAt.IsZero() {
				t.Fatalf("wrong completion fact: %+v", current.Abandoned)
			}
			if len(s.Replies(conversation)) != len(before) {
				t.Fatal("non-required delivery appended a reply")
			}
			pending, err := d.PendingAbandonments(t.Context())
			if err != nil || len(pending) != 0 {
				t.Fatalf("no receiver kept an endless obligation: %v %v", pending, err)
			}
		})
	}
}

type abandonEvidenceReadFailure struct{ ledger.Reader }

func (r abandonEvidenceReadFailure) QueryRow(query string, args ...any) *ledger.Row {
	if len(args) > 0 && args[0] == consoleExchangeKind {
		return r.Reader.QueryRow(`SELECT missing_column FROM absent_delivery_table`)
	}
	return r.Reader.QueryRow(query, args...)
}
func TestAbandonmentAckRequiresCommittedReceiverFacts(t *testing.T) {
	for _, mode := range []string{"not committed", "read failure", "missing receiver control", "wrong receipt", "wrong reply", "wrong terminal state", "paired foreign receipt", "stale decision"} {
		t.Run(mode, func(t *testing.T) {
			s, book, d, r := durableAbandonFixture(t)
			if _, err := d.AbandonAttempt(t.Context(), r.ID, "owner", 1); err != nil {
				t.Fatal(err)
			}
			if err := d.ProjectAbandoned(t.Context(), r.ID); err != nil {
				t.Fatal(err)
			}
			r, _ = d.attempts.Get(t.Context(), r.ID)
			if mode != "not committed" && mode != "read failure" && mode != "missing receiver control" {
				if err := s.deliverAbandonment(t.Context(), r); err != nil {
					t.Fatal(err)
				}
			}
			proof := abandonDeliveryEvidenceTx
			switch mode {
			case "read failure":
				proof = func(tx ledger.Reader, current attempt.Record) (attempt.AbandonDelivery, error) {
					return abandonDeliveryEvidenceTx(abandonEvidenceReadFailure{tx}, current)
				}
			case "missing receiver control":
				abandonDeliverySQL(t, book, `DELETE FROM bindings WHERE kind='console-store'`)
			case "wrong receipt":
				abandonDeliverySQL(t, book, `UPDATE bindings SET data=json_set(data,'$.receipt.attempt_id','another') WHERE kind='console-exchange' AND id='original'`)
			case "wrong reply":
				abandonDeliverySQL(t, book, `UPDATE bindings SET data=json_set(data,'$.text','another reply') WHERE kind='console-reply'`)
			case "wrong terminal state":
				abandonDeliverySQL(t, book, `UPDATE bindings SET data=json_set(data,'$.state','done') WHERE kind='console-exchange' AND id='original'`)
			case "paired foreign receipt":
				abandonDeliverySQL(t, book, `UPDATE bindings SET data=json_set(data,'$.receipt.attempt_id','another') WHERE kind='console-exchange' AND id='original'`)
				abandonDeliverySQL(t, book, `UPDATE bindings SET data=json_set(data,'$.attempt_id','another') WHERE kind='console-reply'`)
			case "stale decision":
				copy := *r.Abandoned
				copy.ForceStopRevision++
				r.Abandoned = &copy
			}
			if err := d.CompleteAbandonDelivery(t.Context(), r, proof); err == nil {
				t.Fatal("unproved receiver state was acknowledged")
			}
			current, _ := d.attempts.Get(t.Context(), r.ID)
			if !current.Abandoned.DeliveryDoneAt.IsZero() {
				t.Fatal("failed evidence changed ack")
			}
		})
	}
}

func TestAbandonmentDeliveryKeepsTheOriginalInputAndDoesNotReappearAfterDeletion(t *testing.T) {
	s, _, d, r := durableAbandonFixture(t)
	s.mu.Lock()
	later := &queuedExchange{Exchange: Exchange{ID: "later", Conversation: "console:delivery", State: consoleapi.ExchangeAwaitingUser, Input: "later task"}, done: make(chan struct{})}
	s.exchanges[later.Conversation] = append(s.exchanges[later.Conversation], later)
	s.running[later.Conversation]++
	err := s.save()
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if out, err := s.Abandon(t.Context(), r.ID, 1); err != nil || out.Pending {
		t.Fatalf("abandonment=%+v %v", out, err)
	}
	if later.State.Terminal() || later.RecoveryAbandon != nil {
		t.Fatal("abandonment ended a later input")
	}
	if err := s.Discard("delivery"); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileAbandonments(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(s.Replies("delivery")) != 0 {
		t.Fatal("settled delivery recreated deleted history")
	}
	pending, err := d.PendingAbandonments(t.Context())
	if err != nil || len(pending) != 0 {
		t.Fatal("settled delivery reentered pending history")
	}
}

func TestRestoredAbandonmentIntentDoesNotUseUncommittedTerminalMemory(t *testing.T) {
	s, book, _, r := durableAbandonFixture(t)
	life, stop := context.WithTimeout(t.Context(), 3*time.Second)
	defer stop()
	s.EnableRetainedRecovery(life)
	s.mu.Lock()
	target := s.exchanges["console:delivery"][0]
	target.RecoveryAbandon = &consoleapi.Reply{AttemptID: r.ID, Text: "abandoned"}
	err := s.save()
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	abandonDeliverySQL(t, book, `CREATE TRIGGER refuse_restored_abandon_reply BEFORE INSERT ON bindings WHEN NEW.kind='console-reply' BEGIN SELECT RAISE(ABORT,'restored reply refused'); END`)
	s.finish(target, *target.RecoveryAbandon, nil)
	if target.State.Terminal() || len(s.Replies("delivery")) != 0 {
		t.Fatal("restored abandonment bypassed rollback")
	}
	abandonDeliverySQL(t, book, `DROP TRIGGER refuse_restored_abandon_reply`)
	s.finish(target, *target.RecoveryAbandon, nil)
	if !target.State.Terminal() || len(s.Replies("delivery")) != 1 {
		t.Fatal("restored abandonment did not retry once")
	}
}

func TestAbandonmentReceiverErrorIsNotGone(t *testing.T) {
	_, book, d, r := durableAbandonFixture(t)
	if _, err := d.AbandonAttempt(t.Context(), r.ID, "owner", 1); err != nil {
		t.Fatal(err)
	}
	if err := d.ProjectAbandoned(t.Context(), r.ID); err != nil {
		t.Fatal(err)
	}
	r, _ = d.attempts.Get(t.Context(), r.ID)
	err := book.Read(context.Background(), func(tx *ledger.ReadTx) error {
		result, err := abandonDeliveryEvidenceTx(abandonEvidenceReadFailure{tx}, r)
		if result == attempt.AbandonDeliveryGone {
			t.Fatal("read error became gone")
		}
		return err
	})
	if err == nil || errors.Is(err, errAbandonDeliveryPending) {
		t.Fatalf("wrong read failure=%v", err)
	}
}
