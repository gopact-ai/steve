package turn

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

func recoveryCopyFixture(t *testing.T, snapshot bool) (*Coordinator, project.Project, attempt.Record, string) {
	t.Helper()
	c, tasks := taskCoordinator(t, &fakeRunner{reply: "unused"}, withOwner("owner"))
	p := project.Project{ID: "recovery", Home: project.Home{Node: "node", Path: t.TempDir()}}
	if err := c.projects.Declare(t.Context(), []project.Project{p}); err != nil {
		t.Fatal(err)
	}
	p, _, _ = c.projects.Get(t.Context(), p.ID)
	if err := os.WriteFile(filepath.Join(p.Home.Path, "original"), []byte("named base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var base string
	if snapshot {
		m, _, err := c.artifacts.SnapshotCanonical(t.Context(), p, "", "fixture", "before original work")
		if err != nil {
			t.Fatal(err)
		}
		base = m.ID
	}
	tracked, err := tasks.Create(task.Task{Channel: "console:recovery", Transport: "console", Member: "worker", ProjectID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tasks.Begin(tracked.ID, "worker", "node", ""); err != nil {
		t.Fatal(err)
	}
	token, _ := tasks.ExecutionToken(tracked.ID)
	r, err := c.attempts.Open(t.Context(), attempt.Spec{ID: "source", Kind: attempt.KindChat, TaskID: tracked.ID, TurnID: "web-original", Execution: &token, Project: p.ID, Node: "node", Harness: "mock", Agent: "worker", Base: base, Scope: attempt.ScopeUnrestricted, Workspace: p.Canonical()})
	if err != nil {
		t.Fatal(err)
	}
	if err := tasks.BindAttempt(token, r.ID, r.TurnID); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []attempt.State{attempt.Prepared, attempt.Running} {
		r, err = c.attempts.Advance(t.Context(), r.ID, phase, "fixture", func(r *attempt.Record) { r.Session = "ns_source" })
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := c.attempts.MarkUnsettled(t.Context(), r.ID, "fixture", errors.New("original writer unreachable"), nil); err != nil {
		t.Fatal(err)
	}
	if err := NewForceStopControl(c).ForceStopAttempt(t.Context(), r.ID, "owner", 0); err != nil {
		t.Fatal(err)
	}
	r, err = c.attempts.RecordForceStopResult(t.Context(), r.ID, 1, true, "stop_unproven")
	if err != nil {
		t.Fatal(err)
	}
	return c, p, r, base
}

func TestAbandonCanonicalPinsItsNamedBaseAndKeepsAnIndependentHold(t *testing.T) {
	c, p, source, base := recoveryCopyFixture(t, true)
	before, _, err := c.artifacts.Resolve(t.Context(), artifact.CanonicalRef(p.ID))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.Home.Path, "original"), []byte("unsettled disk write\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := NewAbandonControl(c).AbandonAttempt(t.Context(), source.ID, "owner", 1)
	if err != nil {
		t.Fatal(err)
	}
	if r.Abandoned.WorkspaceRecoveryID == "" {
		t.Fatal("canonical abandonment has no durable workspace recovery")
	}
	episode, err := c.attempts.WorkspaceRecovery(t.Context(), r.Abandoned.WorkspaceRecoveryID)
	if err != nil {
		t.Fatal(err)
	}
	if episode.Baseline.Artifact != base || episode.Baseline.Name != before.Name || episode.Baseline.Version != before.Version || episode.Head.Artifact != base || episode.Target != p.Home {
		t.Fatalf("recovery did not pin the named base and original target: %+v", episode)
	}
	head, _, _ := c.artifacts.Resolve(t.Context(), before.Name)
	if head != before {
		t.Fatal("abandonment snapshotted or moved the quarantined canonical name")
	}
	proof := attempt.RetainedEvidence{ObservedAt: time.Now(), Session: nodewire.SessionState{ID: r.Session, Harness: r.Harness, ProcessStopped: true, State: nodewire.SessionClosed, Binding: nodewire.SessionBinding{ProjectID: r.Project, SessionID: attempt.RetainedSessionID("console:recovery", r.TaskID, r.Agent), TaskID: r.TaskID, AttemptID: r.ID, NodeID: r.Node, TaskEpoch: r.Execution.Epoch, ExecutionEpoch: attempt.SessionExecutionEpoch(r)}}}
	if _, err := c.attempts.ConfirmTaskStopped(t.Context(), r.ID, "fixture", proof); err != nil {
		t.Fatal(err)
	}
	if err := NewAbandonControl(c).ProjectAbandoned(t.Context(), r.ID); err != nil {
		t.Fatal(err)
	}
	if err := ledgerOf(t, c).Update(t.Context(), func(tx *ledger.Tx) error { return attempt.CheckWriterTx(tx, p.Home.Node, p.Home.Path) }); !errors.Is(err, attempt.ErrWorkspaceRecovery) {
		t.Fatalf("physical exit released the unfinished workspace recovery: %v", err)
	}
	if _, err := c.attempts.Open(t.Context(), attempt.Spec{ID: "replacement", Project: p.ID, Scope: attempt.ScopeUnrestricted, Workspace: p.Canonical()}); !errors.Is(err, attempt.ErrWorkspaceRecovery) {
		t.Fatalf("original directory admitted work before recovery finished: %v", err)
	}
	if _, _, err := c.artifacts.SnapshotCanonical(t.Context(), p, "", "fixture", "must not read old disk"); !errors.Is(err, attempt.ErrWorkspaceRecovery) {
		t.Fatalf("canonical snapshot bypassed the recovery hold: %v", err)
	}
	if _, err := c.artifacts.Bind(t.Context(), before.Name, before.Version, base); !errors.Is(err, attempt.ErrWorkspaceRecovery) {
		t.Fatalf("canonical name moved through its recovery hold: %v", err)
	}
}

func TestAbandonCanonicalWithoutANamedBaseDoesNotReadTheOldDirectory(t *testing.T) {
	c, p, source, _ := recoveryCopyFixture(t, false)
	before, _ := c.tasks.Get(source.TaskID)
	if _, err := NewAbandonControl(c).AbandonAttempt(t.Context(), source.ID, "owner", 1); err == nil {
		t.Fatal("accepted canonical recovery without a named base")
	}
	current, _ := c.attempts.Get(t.Context(), source.ID)
	after, _ := c.tasks.Get(source.TaskID)
	if current.Abandoned != nil || !sameAbandonJSON(before, after) {
		t.Fatal("missing baseline changed the abandonment or task accounting")
	}
	if _, found, err := c.artifacts.Resolve(t.Context(), artifact.CanonicalRef(p.ID)); err != nil || found {
		t.Fatalf("missing base was replaced by a snapshot of quarantined disk: %v %v", found, err)
	}
}

func TestAbandonCanonicalRecoverySharesTheCoreCommit(t *testing.T) {
	c, p, source, _ := recoveryCopyFixture(t, true)
	book := ledgerOf(t, c)
	forceStopTrigger(t, c, `CREATE TRIGGER refuse_workspace_recovery BEFORE INSERT ON operations WHEN NEW.kind='workspace-recovery' BEGIN SELECT RAISE(ABORT,'recovery refused'); END`)
	before, _ := c.tasks.Get(source.TaskID)
	if _, err := NewAbandonControl(c).AbandonAttempt(t.Context(), source.ID, "owner", 1); err == nil {
		t.Fatal("accepted abandonment after its recovery record was refused")
	}
	r, _ := c.attempts.Get(t.Context(), source.ID)
	after, _ := c.tasks.Get(source.TaskID)
	if r.Abandoned != nil || !sameAbandonJSON(before, after) {
		t.Fatal("failed recovery creation partially committed abandonment")
	}
	if err := book.Update(t.Context(), func(tx *ledger.Tx) error { _, err := tx.Exec("DROP TRIGGER refuse_workspace_recovery"); return err }); err != nil {
		t.Fatal(err)
	}
	forceStopTrigger(t, c, `CREATE TRIGGER require_workspace_before_abandon BEFORE UPDATE ON operations WHEN NEW.kind='attempt' AND json_extract(NEW.data,'$.abandoned') IS NOT NULL AND NOT EXISTS(SELECT 1 FROM operations WHERE kind='workspace-recovery') BEGIN SELECT RAISE(ABORT,'abandonment without recovery'); END`)
	if _, err := NewAbandonControl(c).AbandonAttempt(t.Context(), source.ID, "owner", 1); err != nil {
		t.Fatal(err)
	}
	if _, found, err := c.attempts.RecoveryForProject(t.Context(), p.ID); err != nil || !found {
		t.Fatalf("recovery absent after committed abandonment: %v %v", found, err)
	}
}
