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
	if err := c.attempts.MarkUnsettled(t.Context(), r.ID, "fixture", errors.New("original writer is unreachable"), &attempt.Usage{Input: 11, Output: 13, Model: "fixture", Context: 17, Reported: true}); err != nil {
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
	if got.Usage == nil || got.Usage.Context != 17 {
		t.Fatal("abandonment lost already recorded context usage")
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

func TestAbandonmentDoesNotFreezeOrAbandonUnselectedDescendantExecutions(t *testing.T) {
	c, tasks, r, _ := forceStopControlFixture(t)
	if err := tasks.BindAttempt(*r.Execution, r.ID, r.TurnID); err != nil {
		t.Fatal(err)
	}
	child, err := tasks.Create(task.Task{Parent: r.TaskID, Channel: "console:original", Transport: "console", Member: "worker", ProjectID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Begin(child.ID, "worker", "node", ""); err != nil {
		t.Fatal(err)
	}
	token, err := tasks.ExecutionToken(child.ID)
	if err != nil {
		t.Fatal(err)
	}
	spec := r.Spec
	spec.ID = "child-execution"
	spec.TaskID = child.ID
	spec.Execution = &token
	spec.TurnID = "child-input"
	spec.Workspace.ID = "child-workspace"
	spec.Workspace.Path = r.Workspace.Path + "-child"
	other, err := c.attempts.Open(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := tasks.BindAttempt(token, other.ID, other.TurnID); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []attempt.State{attempt.Prepared, attempt.Running} {
		other, err = c.attempts.Advance(t.Context(), other.ID, phase, "fixture", func(r *attempt.Record) { r.Session = "ns_child" })
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := NewForceStopControl(c).ForceStopAttempt(t.Context(), r.ID, "owner", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := c.attempts.RecordForceStopResult(t.Context(), r.ID, 1, true, "stop_unproven"); err != nil {
		t.Fatal(err)
	}
	if _, err := NewAbandonControl(c).AbandonAttempt(t.Context(), r.ID, "owner", 1); err != nil {
		t.Fatal(err)
	}
	loaded, _ := c.attempts.Get(t.Context(), other.ID)
	if loaded.Abandoned != nil || !loaded.Unsettled {
		t.Fatal("the unselected descendant lost its own stop obligation")
	}
	tracked, _ := tasks.Get(child.ID)
	if tracked.State != task.StateCancelled || !tracked.Attempts[0].AccountingFrozenAt.IsZero() || !tracked.Attempts[0].Open() {
		t.Fatal("target abandonment froze a different execution")
	}
	if err := tasks.SettleAttempt(t.Context(), child.ID, other.ID, other.TurnID, time.Now(), task.OutcomeCancelled, task.RecoveryUsage{Tokens: task.Tokens{Input: 9, Output: 8}, Reported: true}); err != nil {
		t.Fatal(err)
	}
	root, _ := tasks.Get(r.TaskID)
	if root.Budget.Tokens.Input != 9 || root.Budget.Tokens.Output != 8 {
		t.Fatal("freezing one execution discarded a descendant's valid accounting")
	}
}

func TestStaleAbandonConfirmationTellsTheOwnerToRefresh(t *testing.T) {
	c, _, r := abandonFixture(t)
	_, err := NewAbandonControl(c).AbandonAttempt(t.Context(), r.ID, "owner", 0)
	var explained UserError
	if !errors.As(err, &explained) || !errors.Is(err, attempt.ErrForceStopChanged) {
		t.Fatalf("stale abandonment is not an actionable refusal: %v", err)
	}
}
