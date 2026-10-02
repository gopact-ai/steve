package console

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
)

type abandonDriverFixture struct {
	calls             int
	id, owner         string
	revision          uint64
	record            attempt.Record
	projectErr, error error
}

func (f *abandonDriverFixture) AbandonAttempt(_ context.Context, id, owner string, revision uint64) (attempt.Record, error) {
	f.calls++
	f.id, f.owner, f.revision = id, owner, revision
	return f.record, f.error
}
func (f *abandonDriverFixture) ProjectAbandoned(context.Context, string) error {
	if f.projectErr != nil {
		return f.projectErr
	}
	f.record.Abandoned.ProjectedAt = time.Now()
	return nil
}
func (f *abandonDriverFixture) PendingAbandonments(context.Context) ([]attempt.Record, error) {
	return []attempt.Record{f.record}, nil
}
func (f *abandonDriverFixture) ReadAbandoned(context.Context, string) (attempt.Record, error) {
	return f.record, nil
}

func TestAbandonConsoleDistinguishesCommittedDecisionFromPendingProjection(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "projected", true: "pending"}[failed], func(t *testing.T) {
			s := New(&echo{}, "owner", nil)
			f := &abandonDriverFixture{record: attempt.Record{Spec: attempt.Spec{ID: "original"}, Abandoned: &attempt.Abandoned{At: time.Now(), By: "owner", ForceStopRevision: 7}}}
			if failed {
				f.projectErr = errors.New("archive unavailable")
			}
			s.SetAbandons(f)
			got, err := s.Abandon(t.Context(), "original", 7)
			if err != nil || !got.Accepted || got.Pending != failed {
				t.Fatalf("result=%+v err=%v", got, err)
			}
			if f.calls != 1 || f.owner != "owner" || f.id != "original" || f.revision != 7 {
				t.Fatalf("abandonment lost its owner or original identity: %+v", f)
			}
		})
	}
}

func TestAbandonConsoleDoesNotAcceptAnUncommittedDecision(t *testing.T) {
	s := New(&echo{}, "owner", nil)
	f := &abandonDriverFixture{error: errors.New("stale confirmation")}
	s.SetAbandons(f)
	got, err := s.Abandon(t.Context(), "original", 3)
	if err == nil || got.Accepted {
		t.Fatal("a rejected core decision was accepted")
	}
}

func TestAbandonmentReplyIsDurableWithoutAStopReceipt(t *testing.T) {
	s := New(&echo{}, "owner", nil)
	doc := &memDoc{}
	if err := s.Persist(doc); err != nil {
		t.Fatal(err)
	}
	target := &queuedExchange{Exchange: Exchange{ID: "old", Conversation: "console:main", State: consoleapi.ExchangeAwaitingUser}, RecoveryStopPending: "unproved", done: make(chan struct{})}
	s.mu.Lock()
	s.exchanges[target.Conversation] = []*queuedExchange{target}
	s.running[target.Conversation] = 1
	s.mu.Unlock()
	if err := s.recordAbandonment(target, "original-attempt"); err != nil {
		t.Fatal(err)
	}
	if target.RecoveryStop != nil || target.RecoveryAbandon == nil || target.State != consoleapi.ExchangeCancelled {
		t.Fatal("abandonment used a physical stop receipt or did not end the exchange")
	}
	restored := New(&echo{}, "owner", nil)
	if err := restored.Persist(doc); err != nil {
		t.Fatal(err)
	}
	row := restored.exchanges[target.Conversation][0]
	fromRecord := durableExchange(row).queued()
	if fromRecord.RecoveryAbandon == nil || fromRecord.RecoveryAbandon.AttemptID != "original-attempt" {
		t.Fatal("record encoding lost abandonment reply")
	}
	if row.RecoveryAbandon == nil || row.RecoveryStop != nil || row.RecoveryAbandon.AttemptID != "original-attempt" {
		t.Fatal("restart lost the distinct abandoned outcome")
	}
}

func TestAbandoningAChildExecutionDoesNotFinishItsParentsExchange(t *testing.T) {
	s := New(&echo{}, "owner", nil)
	if err := s.Persist(&memDoc{}); err != nil {
		t.Fatal(err)
	}
	e := &queuedExchange{Exchange: Exchange{ID: "parent", Conversation: "console:main", State: consoleapi.ExchangeAwaitingUser}, RecoveryStopPending: "pending", done: make(chan struct{})}
	s.mu.Lock()
	s.exchanges[e.Conversation] = []*queuedExchange{e}
	s.running[e.Conversation] = 1
	s.mu.Unlock()
	r := attempt.Record{Spec: attempt.Spec{ID: "child", Kind: attempt.KindDelegate}, Abandoned: &attempt.Abandoned{At: time.Now(), ProjectedAt: time.Now(), Conversation: e.Conversation, MessageID: AnchorMark + e.ID}}
	if err := s.deliverAbandonment(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if e.State != consoleapi.ExchangeAwaitingUser || e.RecoveryAbandon != nil {
		t.Fatal("abandoning a child ended the parent's conversation exchange")
	}
}

func (f *abandonDriverFixture) CompleteAbandonDelivery(ctx context.Context, expected attempt.Record, proof func(ledger.Reader, attempt.Record) (attempt.AbandonDelivery, error)) error {
	return nil
}
