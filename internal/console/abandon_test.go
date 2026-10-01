package console

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
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
