package turn

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/task"
)

func retireOriginalRecoverySource(t *testing.T, c *Coordinator, source attempt.Record) attempt.Record {
	t.Helper()
	tracked, ok := c.tasks.Get(source.TaskID)
	if !ok {
		t.Fatal("source task missing")
	}
	proof := attempt.RetainedEvidence{ObservedAt: time.Now(), Session: nodewire.SessionState{ID: source.Session, Harness: source.Harness, ProcessStopped: true, State: nodewire.SessionClosed, Binding: nodewire.SessionBinding{ProjectID: source.Project, SessionID: attempt.RetainedSessionID(tracked.Channel, source.TaskID, source.Agent), TaskID: source.TaskID, AttemptID: source.ID, NodeID: source.Node, TaskEpoch: source.Execution.Epoch, ExecutionEpoch: attempt.SessionExecutionEpoch(source)}}}
	var err error
	source, err = c.attempts.ConfirmTaskStopped(t.Context(), source.ID, "fixture", proof)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.attempts.MarkStopProjected(t.Context(), source.ID, "fixture"); err != nil {
		t.Fatal(err)
	}
	if err := c.attempts.CompleteAbandonDelivery(t.Context(), source, func(ledger.Reader, attempt.Record) (attempt.AbandonDelivery, error) {
		return attempt.AbandonDeliveryNotRequired, nil
	}); err != nil {
		t.Fatal(err)
	}
	return source
}

func TestRecoveryOriginalSourceTaskMustMatchItsAbandonedAttempt(t *testing.T) {
	c, _, source, _ := sharedCopy(t)
	source = retireOriginalRecoverySource(t, c, source)
	episode, err := c.attempts.WorkspaceRecovery(t.Context(), source.Abandoned.WorkspaceRecoveryID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.tasks.DeleteChannel(t.Context(), "console:recovery", attempt.CheckTaskDeletionTx); !errors.Is(err, task.ErrRetirementPending) {
		t.Fatalf("normal recovery did not retain original source: %v", err)
	} else {
		t.Logf("normal original authority deletion refused: %v", err)
	}
	episode.Sources[0].Task = "unrelated-task"
	raw, err := json.Marshal(episode)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledgerOf(t, c).Update(t.Context(), func(tx *ledger.Tx) error {
		_, err := tx.Exec(`UPDATE operations SET data=? WHERE id=?`, string(raw), episode.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_, decodeErr := c.attempts.WorkspaceRecovery(t.Context(), episode.ID)
	deleted, deleteErr := c.tasks.DeleteChannel(t.Context(), "console:recovery", attempt.CheckTaskDeletionTx)
	if deleteErr == nil {
		t.Fatalf("source task mismatch decoded with err=%v and deleted the real abandoned source: tasks=%v actual=%s claimed=%s", decodeErr, deleted, source.TaskID, episode.Sources[0].Task)
	}
	if decodeErr == nil {
		t.Fatal("source task mismatch remained a trusted recovery")
	}
}

// Multiple historical unconfirmed writers can already exist before recovery.
// The second record is installed as an old owner fact, not by bypassing a new
// writer's admission; both AB decisions use the normal task/attempt transaction.
func twoOriginalRecoverySources(t *testing.T) (*Coordinator, attempt.Record, attempt.Record, attempt.WorkspaceRecovery) {
	t.Helper()
	c, _, first, _ := recoveryCopyFixture(t, true)
	tracked, err := c.tasks.Create(task.Task{Channel: "console:source-two", Transport: "console", Member: first.Agent, ProjectID: first.Project})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.tasks.Begin(tracked.ID, first.Agent, first.Node, ""); err != nil {
		t.Fatal(err)
	}
	token, err := c.tasks.ExecutionToken(tracked.ID)
	if err != nil {
		t.Fatal(err)
	}
	second := first
	second.ID, second.TaskID, second.TurnID = "source-two", tracked.ID, "web-source-two"
	second.Execution, second.Session, second.Leases = &token, "ns_source_two", nil
	second.ForceStop, second.Abandoned = nil, nil
	second.Revision, second.StartedAt = 1, time.Now().UTC()
	raw, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	imported := ledger.Operation{ID: second.ID, Kind: "attempt", State: string(second.State), Revision: second.Revision, Data: raw}
	if _, err := ledgerOf(t, c).BeginGuarded(t.Context(), second.ID, "attempt", string(second.State), "fixture", second, func(tx *ledger.Tx) error {
		return attempt.ImportHistoryTx(tx, []ledger.Operation{imported}, false)
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.tasks.BindAttempt(token, second.ID, second.TurnID); err != nil {
		t.Fatal(err)
	}
	if err := NewForceStopControl(c).ForceStopAttempt(t.Context(), second.ID, "owner", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := c.attempts.RecordForceStopResult(t.Context(), second.ID, 1, true, "stop_unproven"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{first.ID, second.ID} {
		if _, err := NewAbandonControl(c).AbandonAttempt(t.Context(), id, "owner", 1); err != nil {
			t.Fatal(err)
		}
	}
	first, _ = c.attempts.Get(t.Context(), first.ID)
	second, _ = c.attempts.Get(t.Context(), second.ID)
	episode, err := c.attempts.WorkspaceRecovery(t.Context(), first.Abandoned.WorkspaceRecoveryID)
	if err != nil || second.Abandoned.WorkspaceRecoveryID != episode.ID || len(episode.Sources) != 2 {
		t.Fatalf("two original AB decisions did not share one episode: %+v %v", episode, err)
	}
	return c, first, second, episode
}

func TestRecoveryOriginalSourcesAreACompleteExactSet(t *testing.T) {
	for _, which := range []string{"task", "revision", "at", "recovery", "duplicate", "missing-first", "missing-added", "record-missing", "record-broken", "project", "target"} {
		t.Run(which, func(t *testing.T) {
			c, first, second, episode := twoOriginalRecoverySources(t)
			for _, source := range []attempt.Record{first, second} {
				if err := NewAbandonControl(c).ProjectAbandoned(t.Context(), source.ID); err != nil {
					t.Fatal(err)
				}
				retireOriginalRecoverySource(t, c, source)
			}
			if err := ledgerOf(t, c).Update(t.Context(), func(tx *ledger.Tx) error {
				return attempt.CheckTaskDeletionTx(tx, []string{first.TaskID, second.TaskID})
			}); !errors.Is(err, task.ErrRetirementPending) {
				t.Fatalf("uncorrupted original sources were not retained: %v", err)
			}
			var recordSQL string
			switch which {
			case "task":
				episode.Sources[1].Task = "wrong-task"
			case "revision":
				episode.Sources[1].Revision++
			case "at":
				episode.Sources[1].At = episode.Sources[1].At.Add(time.Second)
			case "duplicate":
				episode.Sources[1] = episode.Sources[0]
			case "missing-first":
				episode.Sources = episode.Sources[1:]
			case "missing-added":
				episode.Sources = episode.Sources[:1]
			case "recovery":
				recordSQL = `UPDATE operations SET data=json_set(data,'$.abandoned.workspace_recovery_id','another-recovery') WHERE id=?`
			case "record-missing":
				recordSQL = `DELETE FROM operations WHERE id=?`
			case "record-broken":
				recordSQL = `UPDATE operations SET data='{' WHERE id=?`
			case "project":
				recordSQL = `UPDATE operations SET data=json_set(data,'$.project','another-project') WHERE id=?`
			case "target":
				recordSQL = `UPDATE operations SET data=json_set(data,'$.workspace.path','/another-target') WHERE id=?`
			}
			raw, err := json.Marshal(episode)
			if err != nil {
				t.Fatal(err)
			}
			if err := ledgerOf(t, c).Update(t.Context(), func(tx *ledger.Tx) error {
				if recordSQL != "" {
					_, err := tx.Exec(recordSQL, second.ID)
					return err
				}
				_, err := tx.Exec(`UPDATE operations SET data=? WHERE id=?`, string(raw), episode.ID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := c.attempts.WorkspaceRecovery(t.Context(), episode.ID); err == nil {
				t.Errorf("original-source corruption remained readable: %s", which)
			}
			if err := ledgerOf(t, c).Update(t.Context(), func(tx *ledger.Tx) error {
				return attempt.CheckTaskDeletionTx(tx, []string{second.TaskID})
			}); err == nil {
				t.Errorf("original-source corruption allowed its real task deletion: %s", which)
			}
		})
	}
}

func TestRecoveryOriginalSourceSetAllowsReorderingAndRetiredFacts(t *testing.T) {
	c, first, second, episode := twoOriginalRecoverySources(t)
	for _, source := range []attempt.Record{first, second} {
		if err := NewAbandonControl(c).ProjectAbandoned(t.Context(), source.ID); err != nil {
			t.Fatal(err)
		}
		retireOriginalRecoverySource(t, c, source)
	}
	episode.Sources[0], episode.Sources[1] = episode.Sources[1], episode.Sources[0]
	raw, err := json.Marshal(episode)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledgerOf(t, c).Update(t.Context(), func(tx *ledger.Tx) error {
		_, err := tx.Exec(`UPDATE operations SET data=? WHERE id=?`, string(raw), episode.ID)
		if err != nil {
			return err
		}
		// A mutable force-stop control revision is not the original AB fact.
		_, err = tx.Exec(`UPDATE operations SET data=json_set(data,'$.force_stop.revision',9) WHERE id IN (?,?)`, first.ID, second.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	fresh := attempt.New(ledgerOf(t, c))
	if _, err := fresh.WorkspaceRecovery(t.Context(), episode.ID); err != nil {
		t.Fatalf("legitimate reordered retired source facts were rejected: %v", err)
	}
	if err := ledgerOf(t, c).Update(t.Context(), func(tx *ledger.Tx) error {
		return attempt.CheckTaskDeletionTx(tx, []string{first.TaskID, second.TaskID})
	}); !errors.Is(err, task.ErrRetirementPending) {
		t.Fatalf("retirement released the original recovery authority: %v", err)
	}
}
