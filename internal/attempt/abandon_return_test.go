package attempt

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/task"
)

func TestAbandonedFailureReturnsItsCommittedAccounting(t *testing.T) {
	for _, known := range []bool{false, true} {
		t.Run(map[bool]string{false: "unknown", true: "known"}[known], func(t *testing.T) {
			s, frozen := abandonedReturnFixture(t, known)
			s.now = func() time.Time { return frozen.Abandoned.At.Add(time.Hour) }
			returned, err := s.FailWith(t.Context(), frozen.ID, "late-observer", "observer ended", &Usage{Model: "late-model", Input: 700, Output: 900, Context: 1000, Reported: true})
			if err != nil {
				t.Fatal(err)
			}
			persisted, err := s.Get(t.Context(), frozen.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !sameUsage(persisted.Usage, frozen.Usage) || !persisted.EndedAt.Equal(frozen.EndedAt) {
				t.Fatal("late failure changed frozen durable accounting")
			}
			if !sameUsage(returned.Usage, persisted.Usage) || !returned.EndedAt.Equal(persisted.EndedAt) {
				t.Fatalf("returned accounting differs from committed snapshot: usage=%+v want=%+v; ended=%s want=%s", returned.Usage, persisted.Usage, returned.EndedAt, persisted.EndedAt)
			}
			assertSameReturnedRecord(t, returned, persisted)
		})
	}
}

func TestRejectedAbandonedFailureReturnsNoProposedRecord(t *testing.T) {
	s, frozen := abandonedReturnFixture(t, true)
	if err := s.l.Update(t.Context(), func(tx *ledger.Tx) error {
		_, err := tx.Exec(`CREATE TRIGGER refuse_late_failure BEFORE UPDATE ON operations WHEN NEW.id='att-live' AND NEW.state='failed' BEGIN SELECT RAISE(ABORT,'late failure refused'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	returned, err := s.FailWith(t.Context(), frozen.ID, "late-observer", "observer ended", &Usage{Input: 500, Reported: true})
	if err == nil || returned.ID != "" {
		t.Fatalf("refused transition returned a committed record: %+v %v", returned, err)
	}
	persisted, err := s.Get(t.Context(), frozen.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertSameReturnedRecord(t, frozen, persisted)
}

func TestOrdinaryFailureReturnsItsCommittedUsage(t *testing.T) {
	s, _, original, _, _ := retainedFixture(t)
	usage := &Usage{Model: "actual-model", Input: 21, Output: 34, Context: 55, Reported: true}
	returned, err := s.FailWith(t.Context(), original.ID, "observer", "prompt failed", usage)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := s.Get(t.Context(), original.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !sameUsage(returned.Usage, usage) || returned.EndedAt.IsZero() {
		t.Fatal("ordinary failure lost its recorded spend or end")
	}
	assertSameReturnedRecord(t, returned, persisted)
}

func abandonedReturnFixture(t *testing.T, known bool) (*Service, Record) {
	t.Helper()
	s, _, r, _, tasks := retainedFixture(t)
	if err := tasks.BindAttempt(*r.Execution, r.ID, r.TurnID); err != nil {
		t.Fatal(err)
	}
	if known {
		if err := s.MarkUnsettled(t.Context(), r.ID, "observer", errors.New("native stop is unconfirmed"), &Usage{Model: "original-model", Input: 7, Output: 9, CachedRead: 3, CachedWrite: 4, Context: 17, Reported: true}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tasks.SetAside(r.TaskID, task.StateCancelled); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RequestForceStop(t.Context(), r.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordForceStopResult(t.Context(), r.ID, 1, true, "stop_unproven"); err != nil {
		t.Fatal(err)
	}
	if err := tasks.AbandonExecution(t.Context(), r.TaskID, r.ID, r.TurnID, func(tx *ledger.Tx, row task.Attempt, at time.Time) (task.RecoveryUsage, error) {
		return s.AbandonTx(tx, r.ID, "owner", 1, row, at, AbandonContext{State: "absent"})
	}); err != nil {
		t.Fatal(err)
	}
	frozen, err := s.Get(t.Context(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	return s, frozen
}

func assertSameReturnedRecord(t *testing.T, got, want Record) {
	t.Helper()
	a, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("returned record differs from durable result:\n%s\n%s", a, b)
	}
}
