package turn

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/task"
)

func abandonFixture(t *testing.T) (*Coordinator, *task.Store, attempt.Record) {
	t.Helper()
	c, tasks, r, _ := forceStopControlFixture(t)
	if err := tasks.BindAttempt(*r.Execution, r.ID, r.TurnID); err != nil {
		t.Fatal(err)
	}
	if err := c.attempts.MarkUnsettled(t.Context(), r.ID, "fixture", errors.New("original writer is unreachable"), &attempt.Usage{Input: 11, Output: 13, Model: "fixture", Reported: true}); err != nil {
		t.Fatal(err)
	}
	if err := NewForceStopControl(c).ForceStopAttempt(t.Context(), r.ID, "owner", 0); err != nil {
		t.Fatal(err)
	}
	got, err := c.attempts.RecordForceStopResult(t.Context(), r.ID, 1, true, "stop_unproven")
	if err != nil {
		t.Fatal(err)
	}
	return c, tasks, got
}

func TestAbandonRecordsNoPhysicalStopAndFreezesOnlyItsAccounting(t *testing.T) {
	c, tasks, r := abandonFixture(t)
	before, _ := tasks.Get(r.TaskID)
	got, err := NewAbandonControl(c).AbandonAttempt(t.Context(), r.ID, "owner", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Abandoned == nil || got.Abandoned.By != "owner" || got.Abandoned.ForceStopRevision != 1 || got.Abandoned.Reason != "stop_unproven" || got.Abandoned.At.IsZero() {
		t.Fatalf("missing exact abandonment: %+v", got.Abandoned)
	}
	if !got.Unsettled || got.StopEvidence != r.StopEvidence || !sameAbandonJSON(got.Leases, r.Leases) || attempt.TaskStopConfirmed(got) {
		t.Fatalf("abandonment invented a physical stop: %+v", got)
	}
	for _, lease := range r.Leases {
		current, found, err := ledgerOf(t, c).LeaseOf(t.Context(), lease.Key)
		if err != nil || !found || current.Holder != lease.Holder || current.Epoch != lease.Epoch {
			t.Fatalf("abandonment released its physical fence: %+v, %v", current, err)
		}
	}
	frozen, _ := tasks.Get(r.TaskID)
	if frozen.State != task.StateCancelled || frozen.ExecutionEpoch != before.ExecutionEpoch {
		t.Fatal("abandonment changed an already cancelled task's epoch")
	}
	row := frozen.Attempts[len(frozen.Attempts)-1]
	if row.EndedAt != got.Abandoned.At || row.AccountingFrozenAt != got.Abandoned.At || row.Tokens.Input != 11 || row.Tokens.Output != 13 || row.UsageKnown == nil || !*row.UsageKnown {
		t.Fatalf("accounting not frozen at abandonment: %+v", row)
	}
	if err := tasks.SettleAttempt(t.Context(), r.TaskID, r.ID, r.TurnID, got.Abandoned.At.Add(time.Hour), task.OutcomeOK, task.RecoveryUsage{Tokens: task.Tokens{Input: 200, Output: 300}, Reported: true}); err != nil {
		t.Fatal(err)
	}
	after, _ := tasks.Get(r.TaskID)
	if !sameAbandonJSON(frozen, after) {
		t.Fatal("late usage changed abandoned accounting")
	}
	repeated, err := NewAbandonControl(c).AbandonAttempt(t.Context(), r.ID, "owner", 1)
	if err != nil || !sameAbandonJSON(repeated.Abandoned, got.Abandoned) {
		t.Fatalf("duplicate changed abandonment: %+v %v", repeated.Abandoned, err)
	}
	loaded, err := task.OpenLedger(ledgerOf(t, c))
	if err != nil {
		t.Fatal(err)
	}
	restored, _ := loaded.Get(r.TaskID)
	if !sameAbandonJSON(restored, after) {
		t.Fatal("reopen lost frozen accounting")
	}
}

func TestAbandonRejectedCommitKeepsTaskAndExecutionUnchanged(t *testing.T) {
	for _, refused := range []string{"attempt", "accounting"} {
		t.Run(refused, func(t *testing.T) {
			c, tasks, r := abandonFixture(t)
			before, _ := tasks.Get(r.TaskID)
			statement := `CREATE TRIGGER refuse_abandon BEFORE UPDATE ON operations WHEN NEW.id='force-original' AND json_extract(NEW.data,'$.abandoned') IS NOT NULL BEGIN SELECT RAISE(ABORT,'abandon refused'); END`
			if refused == "accounting" {
				statement = `CREATE TRIGGER refuse_abandon BEFORE UPDATE ON bindings WHEN NEW.kind='task-attempt' BEGIN SELECT RAISE(ABORT,'accounting refused'); END`
			}
			forceStopTrigger(t, c, statement)
			if _, err := NewAbandonControl(c).AbandonAttempt(t.Context(), r.ID, "owner", 1); err == nil {
				t.Fatal("refused write accepted abandonment")
			}
			after, _ := tasks.Get(r.TaskID)
			if !sameAbandonJSON(before, after) {
				t.Fatal("refused write changed task cache")
			}
			current, _ := c.attempts.Get(t.Context(), r.ID)
			if !sameAbandonJSON(r, current) {
				t.Fatal("refused write changed execution")
			}
			loaded, err := task.OpenLedger(ledgerOf(t, c))
			if err != nil {
				t.Fatal(err)
			}
			persisted, _ := loaded.Get(r.TaskID)
			if !sameAbandonJSON(before, persisted) {
				t.Fatal("refused write changed durable accounting")
			}
		})
	}
}

func TestAbandonRequiresTheOwnerAndCurrentExhaustedRevision(t *testing.T) {
	for _, which := range []string{"owner", "revision", "active"} {
		t.Run(which, func(t *testing.T) {
			c, tasks, r := abandonFixture(t)
			who, revision := "owner", uint64(1)
			if which == "owner" {
				who = "other"
			}
			if which == "revision" {
				revision = 0
			}
			if which == "active" {
				if err := NewForceStopControl(c).ForceStopAttempt(t.Context(), r.ID, "owner", 1); err != nil {
					t.Fatal(err)
				}
				revision = 2
			}
			before, _ := tasks.Get(r.TaskID)
			current, _ := c.attempts.Get(t.Context(), r.ID)
			if _, err := NewAbandonControl(c).AbandonAttempt(t.Context(), r.ID, who, revision); err == nil {
				t.Fatal("invalid abandonment accepted")
			}
			after, _ := tasks.Get(r.TaskID)
			final, _ := c.attempts.Get(t.Context(), r.ID)
			if !sameAbandonJSON(before, after) || !sameAbandonJSON(current, final) {
				t.Fatal("rejected abandonment changed state")
			}
		})
	}
}

func sameAbandonJSON(a, b any) bool {
	x, e := json.Marshal(a)
	y, f := json.Marshal(b)
	return e == nil && f == nil && bytes.Equal(x, y)
}
