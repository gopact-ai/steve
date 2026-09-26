package turn

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ability"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

var errResolutionObserver = errors.New("original observer did not confirm stopping")

func unresolvedResolutionScope(t *testing.T, c *Coordinator, key execution.Key) *execution.Scope {
	t.Helper()
	scope, err := c.executions.Begin(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	scope.Finish(errResolutionObserver)
	return scope
}

func rejectResolutionAccounting(t *testing.T, book *ledger.Ledger, taskID string) func() {
	t.Helper()
	// SetAside remains writable; only the real settlement commit fails.
	query := fmt.Sprintf(`CREATE TRIGGER reject_resolution_accounting BEFORE INSERT ON bindings
		WHEN NEW.kind = 'task-attempt' AND json_extract(NEW.data, '$.task_id') = '%s'
		AND json_extract(NEW.data, '$.usage_known') IS NOT NULL
		BEGIN SELECT RAISE(FAIL, 'isolated settlement write failure'); END`, taskID)
	if _, err := book.DB().Exec(query); err != nil {
		t.Fatal(err)
	}
	return func() {
		t.Helper()
		if _, err := book.DB().Exec(`DROP TRIGGER reject_resolution_accounting`); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSetTaskAsideResolvesOnlyAccountedOriginalExecution(t *testing.T) {
	for _, mode := range []string{"settles-first", "settlement-failure", "other-task", "new-epoch"} {
		t.Run(mode, func(t *testing.T) {
			c, _, book, old, _ := retainedChatFixture(t)
			unresolvedResolutionScope(t, c, execution.Key{TaskID: old.TaskID, AttemptID: old.ID, InstanceID: "original"})
			if err := c.attempts.MarkUnsettled(t.Context(), old.ID, "test", errResolutionObserver, &attempt.Usage{Input: 17, Output: 8, Reported: true, Model: "original-model"}); err != nil {
				t.Fatal(err)
			}
			if _, err := c.attempts.ConfirmStopped(t.Context(), old.ID, "test", "original process stop verified"); err != nil {
				t.Fatal(err)
			}
			var restore func()
			switch mode {
			case "settlement-failure":
				restore = rejectResolutionAccounting(t, book, old.TaskID)
			case "other-task":
				other, err := c.tasks.Create(task.Task{Channel: "other", Member: old.Agent})
				if err != nil {
					t.Fatal(err)
				}
				unresolvedResolutionScope(t, c, execution.Key{TaskID: other.ID, AttemptID: old.ID, InstanceID: "other-task"})
			case "new-epoch":
				if _, err := c.tasks.SetAside(old.TaskID, task.StatePaused); err != nil {
					t.Fatal(err)
				}
				if _, err := c.tasks.Advance(old.TaskID, task.StateRunning); err != nil {
					t.Fatal(err)
				}
				unresolvedResolutionScope(t, c, execution.Key{TaskID: old.TaskID, AttemptID: old.ID, InstanceID: "new-epoch"})
			}
			tracked, _ := c.tasks.Get(old.TaskID)
			_, err := c.setTaskAside(t.Context(), "pause", tracked, task.StatePaused, true)
			if mode == "settlement-failure" {
				if err == nil || !strings.Contains(err.Error(), "isolated settlement write failure") {
					t.Errorf("uncommitted accounting was treated as resolved: %v", err)
				}
				if got := c.executions.Active(); !reflect.DeepEqual(got, []string{"original"}) {
					t.Errorf("failed accounting cleared the original owner: %v", got)
				}
				tracked, _ = c.tasks.Get(old.TaskID)
				if !tracked.Attempts[0].Open() {
					t.Fatal("failed settlement changed the task accounting row")
				}
				restore()
				_, err = c.setTaskAside(t.Context(), "retry pause", tracked, task.StatePaused, true)
			}
			var want []string
			if mode == "other-task" || mode == "new-epoch" {
				want = []string{mode}
			}
			if mode == "new-epoch" {
				if !errors.Is(err, errResolutionObserver) {
					t.Errorf("old stop cleared the newer epoch's unresolved error: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if got := c.executions.Active(); !reflect.DeepEqual(got, want) {
				t.Errorf("resolved scopes = %v, want %v", got, want)
			}
			tracked, _ = c.tasks.Get(old.TaskID)
			if tracked.Attempts[0].Open() || tracked.Attempts[0].UsageKnown == nil || tracked.Budget.Tokens.Total != 25 {
				t.Fatalf("native stop was resolved without exact original accounting: %+v", tracked.Attempts)
			}
		})
	}
}

func TestStoppedExecutionResolutionRejectsIncompleteEvidenceAndIdentity(t *testing.T) {
	for _, mode := range []string{"missing-token", "wrong-task-token", "wrong-turn", "wrong-epoch", "missing-stop-evidence", "unsettled", "missing-session-settlement"} {
		t.Run(mode, func(t *testing.T) {
			c, _, _, old, _ := retainedChatFixture(t)
			unresolvedResolutionScope(t, c, execution.Key{TaskID: old.TaskID, AttemptID: old.ID, InstanceID: "original"})
			if err := c.attempts.MarkUnsettled(t.Context(), old.ID, "test", errResolutionObserver, nil); err != nil {
				t.Fatal(err)
			}
			record, err := c.attempts.ConfirmStopped(t.Context(), old.ID, "test", "original process stop verified")
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "missing-token":
				record.Execution = nil
			case "wrong-task-token":
				record.Execution.TaskID = "another-task"
			case "wrong-turn":
				record.TurnID = "another-turn"
			case "wrong-epoch":
				record.Execution.Epoch++
			case "missing-stop-evidence":
				record.StopEvidence = ""
			case "unsettled":
				record.Unsettled = true
			case "missing-session-settlement":
				record.SessionSettled = nil
			}
			before, _ := c.tasks.Get(old.TaskID)
			if err := c.resolveStoppedExecution(t.Context(), record); err == nil {
				t.Fatal("invalid evidence/identity resolved the original execution")
			}
			after, _ := c.tasks.Get(old.TaskID)
			if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(c.executions.Active(), []string{"original"}) {
				t.Fatal("invalid evidence/identity changed accounting or execution ownership")
			}
		})
	}
}

func resolutionRelocationPlan(t *testing.T, c *Coordinator, old attempt.Record) (attempt.Record, attempt.RelocationIntent, attempt.RelocationApproval) {
	t.Helper()
	if err := c.attempts.MarkUnsettled(t.Context(), old.ID, "test", errResolutionObserver, &attempt.Usage{Input: 17, Output: 8, Reported: true, Model: "original-model"}); err != nil {
		t.Fatal(err)
	}
	old, err := c.attempts.Get(t.Context(), old.ID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := c.attempts.RecordRelocation(t.Context(), attempt.RelocationIntent{
		SourceID: old.ID, SourceRevision: old.Revision, TaskEpoch: old.Execution.Epoch,
		Checkpoint: "snapshot", Owner: "owner", CreatedAt: time.Now(), InputDigest: "original-input", Prompt: "continue original",
		Target: attempt.Spec{ID: "replacement", TaskID: old.TaskID, TurnID: old.TurnID, Kind: old.Kind,
			Project: old.Project, Node: "node-b", Harness: old.Harness, Agent: old.Agent, Execution: old.Execution,
			ExecutionGeneration: attempt.SessionExecutionEpoch(old) + 1, NativeCommandID: "new-command",
			Scope: attempt.ScopePathSet, Base: "snapshot",
			Workspace: project.Workspace{ID: "replacement", Project: old.Project, Node: "node-b", Path: t.TempDir(), Kind: project.KindWorktree}},
	})
	if err != nil {
		t.Fatal(err)
	}
	approval := relocationApproval(plan, "confirm-stopped-and-retry:"+plan.ID, "owner", nil)
	return old, plan, approval
}

func TestRelocationResolutionPreservesAccountingAndReplacement(t *testing.T) {
	for _, entry := range []string{"open", "replay", "already-bound", "new-epoch"} {
		t.Run(entry, func(t *testing.T) {
			c, _, book, old, _ := retainedChatFixture(t)
			unresolvedResolutionScope(t, c, execution.Key{TaskID: old.TaskID, AttemptID: old.ID, InstanceID: "original"})
			old, plan, approval := resolutionRelocationPlan(t, c, old)
			var replacement attempt.Record
			var err error
			if entry != "open" {
				replacement, err = c.attempts.OpenRelocation(t.Context(), c.text, plan.ID, approval)
				if err != nil {
					t.Fatal(err)
				}
			}
			if entry == "already-bound" || entry == "new-epoch" {
				replacement, err = c.bindRelocation(t.Context(), replacement, old, plan, ability.Admission{})
				if err != nil {
					t.Fatal(err)
				}
				if entry == "new-epoch" {
					if _, err := c.tasks.SetAside(old.TaskID, task.StatePaused); err != nil {
						t.Fatal(err)
					}
					if _, err := c.tasks.Advance(old.TaskID, task.StateRunning); err != nil {
						t.Fatal(err)
					}
					unresolvedResolutionScope(t, c, execution.Key{TaskID: old.TaskID, AttemptID: old.ID, InstanceID: "new-epoch"})
				} else {
					unresolvedResolutionScope(t, c, execution.Key{TaskID: old.TaskID, AttemptID: replacement.ID, InstanceID: "replacement"})
				}
			}
			before, _ := c.tasks.Get(old.TaskID)
			var restore func()
			if entry == "open" || entry == "replay" {
				restore = rejectResolutionAccounting(t, book, old.TaskID)
			}
			if entry == "open" {
				_, err = c.openRelocationAttempt(t.Context(), plan, old, nil, approval)
			} else {
				source, loadErr := c.attempts.Get(t.Context(), old.ID)
				if loadErr != nil {
					t.Fatal(loadErr)
				}
				_, _, _, err = c.relocationPreparation(t.Context(), plan, source)
			}
			if restore != nil {
				if err == nil || !strings.Contains(err.Error(), "isolated settlement write failure") {
					t.Errorf("relocation %s resolved before accounting committed: %v", entry, err)
				}
				if got := c.executions.Active(); !reflect.DeepEqual(got, []string{"original"}) {
					t.Errorf("relocation %s cleared the unaccounted source: %v", entry, got)
				}
				after, _ := c.tasks.Get(old.TaskID)
				if !reflect.DeepEqual(before, after) {
					t.Fatal("failed accounting changed the task")
				}
				restore()
				source, loadErr := c.attempts.Get(t.Context(), old.ID)
				if loadErr != nil {
					t.Fatal(loadErr)
				}
				_, _, _, err = c.relocationPreparation(t.Context(), plan, source)
			}
			if err != nil {
				t.Fatal(err)
			}
			if entry == "already-bound" || entry == "new-epoch" {
				after, _ := c.tasks.Get(old.TaskID)
				if !reflect.DeepEqual(before, after) || len(after.Attempts) != 2 || !after.Attempts[1].Open() {
					t.Fatal("source resolution rebound the workspace, charged twice or closed the replacement")
				}
				name := "replacement"
				if entry == "new-epoch" {
					name = "new-epoch"
				}
				if got := c.executions.Active(); !reflect.DeepEqual(got, []string{name}) {
					t.Fatalf("source resolution changed another execution: %v", got)
				}
				return
			}
			if got := c.executions.Active(); len(got) != 0 {
				t.Fatalf("committed source accounting did not resolve its owner: %v", got)
			}
			replacement, err = c.attempts.Get(t.Context(), plan.Target.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.bindRelocation(t.Context(), replacement, old, plan, ability.Admission{}); err != nil {
				t.Fatal(err)
			}
			after, _ := c.tasks.Get(old.TaskID)
			if len(after.Attempts) != 2 || after.Attempts[0].Open() || !after.Attempts[1].Open() || after.Budget.Tokens.Total != 25 || after.Budget.Turns != before.Budget.Turns {
				t.Fatalf("relocation retry did not preserve source accounting and new turn: %+v", after)
			}
		})
	}
}
