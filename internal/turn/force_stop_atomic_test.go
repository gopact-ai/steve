package turn

import (
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

func forceStopControlFixture(t *testing.T) (*Coordinator, *task.Store, attempt.Record, attempt.RetainedEvidence) {
	t.Helper()
	c, tasks := taskCoordinator(t, &fakeRunner{reply: "unused"}, withOwner("owner"))
	tracked, err := tasks.Create(task.Task{Channel: "console:original", Transport: "console", Member: "worker", ProjectID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Begin(tracked.ID, "worker", "node", ""); err != nil {
		t.Fatal(err)
	}
	token, _ := tasks.ExecutionToken(tracked.ID)
	r, err := c.attempts.Open(t.Context(), attempt.Spec{ID: "force-original", Kind: attempt.KindChat, TaskID: tracked.ID, TurnID: "original-input", Execution: &token, Project: "p", Node: "node", Harness: "mock", Agent: "worker", Scope: attempt.ScopePathSet, Workspace: project.Workspace{ID: "original", Project: "p", Path: t.TempDir(), Node: "node", Kind: project.KindWorktree}})
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []attempt.State{attempt.Prepared, attempt.Running} {
		r, err = c.attempts.Advance(t.Context(), r.ID, phase, "fixture", func(r *attempt.Record) { r.Session = "ns_original" })
		if err != nil {
			t.Fatal(err)
		}
	}
	proof := attempt.RetainedEvidence{ObservedAt: time.Now(), Session: nodewire.SessionState{ID: r.Session, Harness: r.Harness, State: nodewire.SessionIdle, InputAccepted: 1, Binding: nodewire.SessionBinding{ProjectID: r.Project, SessionID: attempt.RetainedSessionID(tracked.Channel, tracked.ID, r.Agent), TaskID: tracked.ID, AttemptID: r.ID, NodeID: r.Node, ExecutionEpoch: attempt.SessionExecutionEpoch(r), TaskEpoch: token.Epoch}, Command: &nodewire.SessionCommand{ID: r.TurnID, InputSequence: 1, State: nodewire.SessionCommandCancelled, Settled: true}}}
	return c, tasks, r, proof
}

func forceStopTrigger(t *testing.T, c *Coordinator, sql string) {
	t.Helper()
	if err := ledgerOf(t, c).Update(t.Context(), func(tx *ledger.Tx) error { _, err := tx.Exec(sql); return err }); err != nil {
		t.Fatal(err)
	}
}

func TestForceStopCancellationAndIntentCommitTogether(t *testing.T) {
	c, tasks, r, proof := forceStopControlFixture(t)
	// A revocation must never become visible before its process-exit request.
	forceStopTrigger(t, c, `CREATE TRIGGER require_force_before_cancel BEFORE UPDATE ON bindings WHEN NEW.kind='task' AND json_extract(NEW.data,'$.state')='cancelled' AND NOT EXISTS(SELECT 1 FROM operations WHERE id='force-original' AND json_extract(data,'$.force_stop.level')='kill') BEGIN SELECT RAISE(ABORT,'revocation without force intent'); END`)
	if err := NewForceStopControl(c).ForceStopAttempt(t.Context(), r.ID, "owner"); err != nil {
		t.Fatalf("force cancellation was not atomic: %v", err)
	}
	loaded, err := task.OpenLedger(ledgerOf(t, c))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := loaded.Get(r.TaskID)
	current, _ := c.attempts.Get(t.Context(), r.ID)
	if got.State != task.StateCancelled || current.ForceStop == nil || current.ForceStop.Level != "kill" {
		t.Fatal("reopen lost cancellation or force intent")
	}
	if _, err := c.attempts.ConfirmTaskStopped(t.Context(), r.ID, "old-ordinary-stop", proof); err == nil {
		t.Fatal("in-flight command receipt relaxed the force stop")
	}
	before, _ := tasks.Get(r.TaskID)
	if err := NewForceStopControl(c).ForceStopAttempt(t.Context(), r.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	after, _ := tasks.Get(r.TaskID)
	current, _ = c.attempts.Get(t.Context(), r.ID)
	if before.ExecutionEpoch != after.ExecutionEpoch || current.ForceStop.Revision != 2 {
		t.Fatal("retry changed the task epoch or lost its revision")
	}
}

func TestForceStopRejectedWriteRollsBackTaskAndIntent(t *testing.T) {
	for _, denied := range []string{"intent", "task"} {
		t.Run(denied, func(t *testing.T) {
			c, tasks, r, _ := forceStopControlFixture(t)
			before, _ := tasks.Get(r.TaskID)
			statement := `CREATE TRIGGER refuse_force BEFORE UPDATE ON operations WHEN NEW.id='force-original' AND json_extract(NEW.data,'$.force_stop') IS NOT NULL BEGIN SELECT RAISE(ABORT,'force refused'); END`
			if denied == "task" {
				statement = `CREATE TRIGGER refuse_force BEFORE UPDATE ON bindings WHEN NEW.kind='task-store' BEGIN SELECT RAISE(ABORT,'task refused'); END`
			}
			forceStopTrigger(t, c, statement)
			if err := NewForceStopControl(c).ForceStopAttempt(t.Context(), r.ID, "owner"); err == nil {
				t.Fatal("refused write accepted force stop")
			}
			loaded, err := task.OpenLedger(ledgerOf(t, c))
			if err != nil {
				t.Fatal(err)
			}
			for _, source := range []*task.Store{tasks, loaded} {
				after, _ := source.Get(r.TaskID)
				if after.State != before.State || after.ExecutionEpoch != before.ExecutionEpoch {
					t.Fatal("failed force request left a task revocation")
				}
			}
			current, _ := c.attempts.Get(t.Context(), r.ID)
			if current.ForceStop != nil || current.Unsettled {
				t.Fatal("failed request left a force intent")
			}
		})
	}
}

func TestForceStopDistinguishesEarlierCommandAndProcessReceipts(t *testing.T) {
	for _, exited := range []bool{false, true} {
		t.Run(map[bool]string{false: "command", true: "process"}[exited], func(t *testing.T) {
			c, tasks, r, proof := forceStopControlFixture(t)
			if _, err := tasks.SetAside(r.TaskID, task.StateCancelled); err != nil {
				t.Fatal(err)
			}
			proof.Session.ProcessStopped = exited
			if _, err := c.attempts.ConfirmTaskStopped(t.Context(), r.ID, "earlier-stop", proof); err != nil {
				t.Fatal(err)
			}
			before, _ := tasks.Get(r.TaskID)
			if err := NewForceStopControl(c).ForceStopAttempt(t.Context(), r.ID, "owner"); err != nil {
				t.Fatalf("known original could not be forced: %v", err)
			}
			got, _ := c.attempts.Get(t.Context(), r.ID)
			if !exited && (got.ForceStop == nil || got.ForceStop.Level != "kill" || !got.Unsettled || !attempt.TaskStopOwed(got)) {
				t.Fatal("command completion was accepted as process exit")
			}
			if exited && (got.Unsettled || got.ForceStop != nil && got.ForceStop.Level != "confirmed") {
				t.Fatal("already exited process was forced again")
			}
			after, _ := tasks.Get(r.TaskID)
			if after.State != before.State || after.ExecutionEpoch != before.ExecutionEpoch {
				t.Fatal("earlier receipt retry changed task authority")
			}
		})
	}
}

func TestOldForceTargetDoesNotCancelANewSessionBinding(t *testing.T) {
	for _, exited := range []bool{false, true} {
		t.Run(map[bool]string{false: "command", true: "process"}[exited], func(t *testing.T) {
			c, tasks, old, proof := forceStopControlFixture(t)
			_, _ = tasks.SetAside(old.TaskID, task.StatePaused)
			proof.Session.ProcessStopped = exited
			if _, err := c.attempts.ConfirmTaskStopped(t.Context(), old.ID, "ordinary-stop", proof); err != nil {
				t.Fatal(err)
			}
			if _, err := tasks.FinishUnstarted(old.TaskID, task.OutcomeCancelled); err != nil {
				t.Fatal(err)
			}
			if _, err := tasks.Advance(old.TaskID, task.StateRunning); err != nil {
				t.Fatal(err)
			}
			if _, err := tasks.Begin(old.TaskID, "worker", "node", ""); err != nil {
				t.Fatal(err)
			}
			child, err := tasks.Spawn(old.TaskID, task.Task{Member: "child", ProjectID: "p"})
			if err != nil {
				t.Fatal(err)
			}
			token, _ := tasks.ExecutionToken(old.TaskID)
			spec := old.Spec
			spec.ID = "replacement"
			spec.TurnID = "next-input"
			spec.Execution = &token
			next, err := c.attempts.Open(t.Context(), spec)
			if err != nil {
				t.Fatal(err)
			}
			for _, phase := range []attempt.State{attempt.Prepared, attempt.Running} {
				next, err = c.attempts.Advance(t.Context(), next.ID, phase, "fixture", func(r *attempt.Record) { r.Session = old.Session })
				if err != nil {
					t.Fatal(err)
				}
			}
			before, _ := tasks.Get(old.TaskID)
			err = NewForceStopControl(c).ForceStopAttempt(t.Context(), old.ID, "owner")
			if !exited && err == nil {
				t.Fatal("old force target accepted a replaced native binding")
			}
			if exited && err != nil {
				t.Fatalf("proved old exit was not an idempotent no-op: %v", err)
			}
			after, _ := tasks.Get(old.TaskID)
			afterChild, _ := tasks.Get(child.ID)
			if after.State != before.State || after.ExecutionEpoch != before.ExecutionEpoch || afterChild.State != child.State || afterChild.ExecutionEpoch != child.ExecutionEpoch {
				t.Fatal("old target revoked new work or its descendants")
			}
			for _, id := range []string{old.ID, next.ID} {
				got, _ := c.attempts.Get(t.Context(), id)
				if got.ForceStop != nil {
					t.Fatal("old target installed force intent on another binding")
				}
			}
		})
	}
}

func TestForceStopTreeWriteFailureIsAtomic(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(map[bool]string{false: "committed", true: "refused-child"}[reject], func(t *testing.T) {
			c, tasks, root, _ := forceStopControlFixture(t)
			child, err := tasks.Spawn(root.TaskID, task.Task{Member: "child-worker", ProjectID: "p"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tasks.Begin(child.ID, "child-worker", "node", ""); err != nil {
				t.Fatal(err)
			}
			token, _ := tasks.ExecutionToken(child.ID)
			spec := root.Spec
			spec.ID = "force-child"
			spec.TaskID = child.ID
			spec.TurnID = "child-input"
			spec.Agent = "child-worker"
			spec.Execution = &token
			spec.Workspace.Path = t.TempDir()
			spec.Workspace.ID = "child-workspace"
			r, err := c.attempts.Open(t.Context(), spec)
			if err != nil {
				t.Fatal(err)
			}
			for _, phase := range []attempt.State{attempt.Prepared, attempt.Running} {
				r, err = c.attempts.Advance(t.Context(), r.ID, phase, "fixture", func(r *attempt.Record) { r.Session = "ns_child" })
				if err != nil {
					t.Fatal(err)
				}
			}
			if reject {
				forceStopTrigger(t, c, `CREATE TRIGGER refuse_child_force BEFORE UPDATE ON operations WHEN NEW.id='force-child' AND json_extract(NEW.data,'$.force_stop') IS NOT NULL BEGIN SELECT RAISE(ABORT,'child force refused'); END`)
			}
			err = NewForceStopControl(c).ForceStopAttempt(t.Context(), root.ID, "owner")
			if reject != (err != nil) {
				t.Fatalf("force=%v rejected=%v", err, reject)
			}
			for _, record := range []attempt.Record{root, r} {
				tracked, _ := tasks.Get(record.TaskID)
				got, _ := c.attempts.Get(t.Context(), record.ID)
				if reject {
					if tracked.State != task.StateRunning || got.ForceStop != nil {
						t.Fatal("rejected tree partially installed")
					}
				} else if tracked.State != task.StateCancelled || got.ForceStop == nil || !got.Unsettled {
					t.Fatal("tree cancellation omitted an original execution")
				}
			}
		})
	}
}
