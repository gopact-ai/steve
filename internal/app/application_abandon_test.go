package app

import (
	"context"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/task"
)

func (s *forceSessions) AbortRetainedSession(ctx context.Context, _ harness.Placement, id, _ string) (nodewire.SessionState, error) {
	return (forceRunner{s: s, id: id}).StopRetained(ctx)
}

func TestAbandonedExhaustedExecutionStillGetsAnOrdinaryPhysicalStop(t *testing.T) {
	s, records, sessions := forceFixture(t, 1)
	r := records[0]
	if _, err := s.attempts.RequestForceStop(t.Context(), r.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.attempts.RecordForceStopResult(t.Context(), r.ID, 1, true, "stop_unproven"); err != nil {
		t.Fatal(err)
	}
	if err := s.tasks.AbandonExecution(t.Context(), r.TaskID, r.ID, r.TurnID, func(tx *ledger.Tx, row task.Attempt, at time.Time) (task.RecoveryUsage, error) {
		return s.attempts.AbandonTx(tx, r.ID, "owner", 1, row, at)
	}); err != nil {
		t.Fatal(err)
	}
	before, _ := s.tasks.Get(r.TaskID)
	if err := s.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, err := s.attempts.Get(t.Context(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !attempt.TaskStopConfirmed(got) || got.Unsettled {
		t.Fatalf("abandoned writer was not physically settled: %+v", got)
	}
	if len(sessions.aborted) != 1 || len(sessions.killed) != 0 {
		t.Fatalf("abandoned cleanup kill=%v abort=%v", sessions.killed, sessions.aborted)
	}
	after, _ := s.tasks.Get(r.TaskID)
	if before.Budget != after.Budget {
		t.Fatal("physical confirmation charged abandoned usage")
	}
	if !after.Attempts[len(after.Attempts)-1].EndedAt.Equal(before.Attempts[len(before.Attempts)-1].EndedAt) {
		t.Fatal("physical confirmation extended frozen duration")
	}
}
