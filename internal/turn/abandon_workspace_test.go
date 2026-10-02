package turn

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

func TestCanonicalRecoveryAndItsHoldShareTheReplicatedAbandonDecision(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		t.Run(map[bool]string{false: "commit", true: "reject"}[rejected], func(t *testing.T) {
			c, p, source, _ := recoveryCopyFixture(t, true)
			book := ledgerOf(t, c)
			replica := &forceControlReplication{book: book, entered: make(chan struct{}), release: make(chan struct{})}
			if rejected {
				replica.reject = errors.New("no quorum")
			}
			if err := book.AttachReplication(replica); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			var joined sync.WaitGroup
			joined.Add(1)
			go func() {
				defer joined.Done()
				_, err := NewAbandonControl(c).AbandonAttempt(ctx, source.ID, "owner", 1)
				done <- err
			}()
			defer func() {
				select {
				case <-replica.release:
				default:
					close(replica.release)
				}
				joined.Wait()
			}()
			select {
			case <-replica.entered:
			case err := <-done:
				t.Fatalf("no proposal: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			before, err := c.attempts.Get(ctx, source.ID)
			if err != nil || before.Abandoned != nil {
				t.Fatalf("uncommitted abandonment visible: %+v %v", before, err)
			}
			if _, found, err := c.attempts.RecoveryForProject(ctx, p.ID); err != nil || found {
				t.Fatalf("uncommitted recovery visible: %v %v", found, err)
			}
			close(replica.release)
			err = <-done
			if rejected && !errors.Is(err, replica.reject) || !rejected && err != nil {
				t.Fatalf("decision: %v", err)
			}
			fresh := attempt.New(book)
			current, err := fresh.Get(ctx, source.ID)
			if err != nil || (current.Abandoned == nil) != rejected {
				t.Fatalf("abandon decision changed after reopen: %+v %v", current, err)
			}
			recovery, found, err := fresh.RecoveryForProject(ctx, p.ID)
			if err != nil || found == rejected || found && recovery.ID != current.Abandoned.WorkspaceRecoveryID {
				t.Fatalf("recovery did not share the abandon commit: %+v %v %v", recovery, found, err)
			}
			loaded, err := task.OpenLedger(book)
			if err != nil {
				t.Fatal(err)
			}
			for _, tasks := range []*task.Store{c.tasks, loaded} {
				tracked, _ := tasks.Get(source.TaskID)
				if tracked.Attempts[0].AccountingFrozenAt.IsZero() != rejected {
					t.Fatal("accounting and recovery decided separately")
				}
			}
		})
	}
}

func TestRecoveryHoldSurvivesConcurrentStopSnapshotLandingAndAdmission(t *testing.T) {
	c, p, source, base := recoveryCopyFixture(t, true)
	r, err := NewAbandonControl(c).AbandonAttempt(t.Context(), source.ID, "owner", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := NewAbandonControl(c).ProjectAbandoned(t.Context(), source.ID); err != nil {
		t.Fatal(err)
	}
	proof := attempt.RetainedEvidence{ObservedAt: time.Now(), Session: nodewire.SessionState{ID: r.Session, Harness: r.Harness, ProcessStopped: true, State: nodewire.SessionClosed, Binding: nodewire.SessionBinding{ProjectID: r.Project, SessionID: attempt.RetainedSessionID("console:recovery", r.TaskID, r.Agent), TaskID: r.TaskID, AttemptID: r.ID, NodeID: r.Node, TaskEpoch: r.Execution.Epoch, ExecutionEpoch: attempt.SessionExecutionEpoch(r)}}}
	start := make(chan struct{})
	stop := make(chan error, 1)
	refusals := make(chan error, 3)
	var joined sync.WaitGroup
	launch := func(f func() error, result chan error) {
		joined.Add(1)
		go func() { defer joined.Done(); <-start; result <- f() }()
	}
	launch(func() error { _, err := c.attempts.ConfirmTaskStopped(t.Context(), r.ID, "fixture", proof); return err }, stop)
	launch(func() error {
		_, err := c.attempts.Open(t.Context(), attempt.Spec{ID: "replacement", Project: p.ID, Node: p.Home.Node, Scope: attempt.ScopeUnrestricted, Workspace: p.Canonical()})
		return err
	}, refusals)
	launch(func() error {
		_, _, err := c.artifacts.SnapshotCanonical(t.Context(), p, "", "fixture", "blocked snapshot")
		return err
	}, refusals)
	launch(func() error { _, err := c.artifacts.Land(t.Context(), p, base, "fixture"); return err }, refusals)
	close(start)
	joined.Wait()
	if err := <-stop; err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := <-refusals; err == nil {
			t.Fatal("stop confirmation opened a canonical write window")
		}
	}
	if err := ledgerOf(t, c).Update(t.Context(), func(tx *ledger.Tx) error { return attempt.CheckWriterTx(tx, p.Home.Node, p.Home.Path) }); !errors.Is(err, attempt.ErrWorkspaceRecovery) {
		t.Fatalf("original hold disappeared after exit: %v", err)
	}
	if raw, err := os.ReadFile(filepath.Join(p.Home.Path, "original")); err != nil || string(raw) != "named base\n" {
		t.Fatalf("blocked canonical operation changed disk: %q %v", raw, err)
	}
}

func TestRecoveryDeletionAndSweepKeepSourceAndAcceptedProducerAuthority(t *testing.T) {
	c, p, source, ws := sharedCopy(t)
	proof := attempt.RetainedEvidence{ObservedAt: time.Now(), Session: nodewire.SessionState{ID: source.Session, Harness: source.Harness, ProcessStopped: true, State: nodewire.SessionClosed, Binding: nodewire.SessionBinding{ProjectID: source.Project, SessionID: attempt.RetainedSessionID("console:recovery", source.TaskID, source.Agent), TaskID: source.TaskID, AttemptID: source.ID, NodeID: source.Node, TaskEpoch: source.Execution.Epoch, ExecutionEpoch: attempt.SessionExecutionEpoch(source)}}}
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
	if _, err := c.tasks.DeleteChannel(t.Context(), "console:recovery", attempt.CheckTaskDeletionTx); !errors.Is(err, task.ErrRetirementPending) || !strings.Contains(err.Error(), "workspace recovery") {
		t.Fatalf("source authority was not retained by its recovery: %v", err)
	}
	first := runCopyAttempt(t, c, recoveryCopySpec(t, c, p, ws, "retained-producer"))
	if err := os.WriteFile(filepath.Join(ws.Path, "head"), []byte("accepted data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := c.attempts.MarkSessionSettled(t.Context(), first.ID, "fixture"); err != nil {
		t.Fatal(err)
	}
	first, _ = c.attempts.Get(t.Context(), first.ID)
	completion, _, err := c.completion(t.Context(), first, Result{Text: "done"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.attempts.FinishCompletion(t.Context(), first.ID, "fixture", completion); err != nil {
		t.Fatal(err)
	}
	if _, err := c.tasks.Finish(first.TaskID, task.OutcomeOK, task.Tokens{}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := c.tasks.DeleteChannel(t.Context(), "console:retained-producer", attempt.CheckTaskDeletionTx); !errors.Is(err, task.ErrRetirementPending) || !strings.Contains(err.Error(), "recovery output") {
		t.Fatalf("accepted head lost its producing authority: %v", err)
	}
	projects := project.Open(ledgerOf(t, c), artifact.CheckDeclarationsTx, attempt.CheckDeclarationsTx)
	changed := p
	changed.Home.Path = t.TempDir()
	if err := projects.Declare(t.Context(), []project.Project{changed}); !errors.Is(err, attempt.ErrWorkspaceRecovery) {
		t.Fatalf("home change discarded the held target: %v", err)
	}
	if err := projects.Retire(t.Context(), p.ID); !errors.Is(err, attempt.ErrWorkspaceRecovery) {
		t.Fatalf("project deletion discarded recovery authority: %v", err)
	}
	container := filepath.Dir(ws.Path)
	old := time.Now().Add(-2 * artifact.SweepAge)
	if err := os.Chtimes(container, old, old); err != nil {
		t.Fatal(err)
	}
	removed, err := c.artifacts.SweepWorktrees(t.Context(), ws.Node, filepath.Dir(filepath.Dir(container)), nil)
	if err != nil || len(removed) != 0 {
		t.Fatalf("between-turn recovery was swept: %v %v", removed, err)
	}
	if raw, err := os.ReadFile(filepath.Join(ws.Path, "head")); err != nil || string(raw) != "accepted data" {
		t.Fatalf("sweep lost accepted work: %q %v", raw, err)
	}
	if err := ledgerOf(t, c).Update(t.Context(), func(tx *ledger.Tx) error {
		_, err := tx.Exec(`UPDATE operations SET data='{' WHERE id=?`, source.Abandoned.WorkspaceRecoveryID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.artifacts.SweepWorktrees(t.Context(), ws.Node, filepath.Dir(filepath.Dir(container)), nil); err == nil {
		t.Fatal("invalid retention owner was treated as an empty keep set")
	}
	if _, err := os.Stat(ws.Path); err != nil {
		t.Fatal("failed retention read deleted the recovery copy")
	}
}

func TestRecoveryCopyRejectsOverlappingProjectDeclarationsWithoutALiveWriter(t *testing.T) {
	for _, shape := range []string{"work", "container", "ancestor", "descendant", "copy"} {
		t.Run(shape, func(t *testing.T) {
			c, _, _, ws := sharedCopy(t)
			projects := project.Open(ledgerOf(t, c), artifact.CheckDeclarationsTx, attempt.CheckDeclarationsTx)
			other := project.Project{ID: "other", Home: project.Home{Node: ws.Node, Path: ws.Path}}
			switch shape {
			case "container":
				other.Home.Path = filepath.Dir(ws.Path)
			case "ancestor":
				other.Home.Path = filepath.Dir(filepath.Dir(ws.Path))
			case "descendant":
				other.Home.Path = filepath.Join(ws.Path, "nested")
			case "copy":
				other.Home = project.Home{Node: "elsewhere", Path: t.TempDir()}
				other.Copies = map[string]project.Copy{ws.Node: {Path: ws.Path}}
			}
			if err := projects.Declare(t.Context(), []project.Project{other}); !errors.Is(err, attempt.ErrWorkspaceRecovery) {
				t.Fatalf("another declaration acquired the recovery copy between turns: %v", err)
			}
			if _, found, err := projects.Get(t.Context(), other.ID); err != nil || found {
				t.Fatalf("refused declaration partially installed: %v %v", found, err)
			}
		})
	}
}

func TestRecoveryCopyDeclarationGuardAllowsAnotherNodeAndAdjacentNames(t *testing.T) {
	c, _, source, ws := sharedCopy(t)
	projects := project.Open(ledgerOf(t, c), artifact.CheckDeclarationsTx, attempt.CheckDeclarationsTx)
	for _, p := range []project.Project{{ID: "neighbor", Home: project.Home{Node: ws.Node, Path: filepath.Dir(ws.Path) + "-neighbor"}}, {ID: "elsewhere", Home: project.Home{Node: "elsewhere", Path: ws.Path}}} {
		if err := projects.Declare(t.Context(), []project.Project{p}); err != nil {
			t.Fatalf("unrelated declaration was rejected: %v", err)
		}
	}
	if err := ledgerOf(t, c).Update(t.Context(), func(tx *ledger.Tx) error {
		_, err := tx.Exec(`UPDATE operations SET data='{' WHERE id=?`, source.Abandoned.WorkspaceRecoveryID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := projects.Declare(t.Context(), []project.Project{{ID: "unrelated", Home: project.Home{Node: "elsewhere", Path: t.TempDir()}}}); err == nil {
		t.Fatal("unreadable recovery metadata admitted a project declaration")
	}
}

func TestRecoveryCopySelectionRejectsAnEarlierProjectDeclaration(t *testing.T) {
	c, _, source, _ := recoveryCopyFixture(t, true)
	r, err := NewAbandonControl(c).AbandonAttempt(t.Context(), source.ID, "owner", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := NewAbandonControl(c).ProjectAbandoned(t.Context(), r.ID); err != nil {
		t.Fatal(err)
	}
	episode, err := c.attempts.WorkspaceRecovery(t.Context(), r.Abandoned.WorkspaceRecoveryID)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := c.artifacts.PlanRecoveryWorkspace(t.Context(), episode, "node")
	if err != nil {
		t.Fatal(err)
	}
	projects := project.Open(ledgerOf(t, c), artifact.CheckDeclarationsTx, attempt.CheckDeclarationsTx)
	if err := projects.Declare(t.Context(), []project.Project{{ID: "earlier", Home: project.Home{Node: ws.Node, Path: ws.Path}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.attempts.SelectRecoveryWorkspace(t.Context(), episode.ID, ws, "owner"); err == nil {
		t.Fatal("recovery binding took a directory already owned by a project")
	}
	current, err := c.attempts.WorkspaceRecovery(t.Context(), episode.ID)
	if err != nil || current.Phase != "recorded" || current.Workspace.Path != "" {
		t.Fatalf("refused binding changed preparation identity: %+v %v", current, err)
	}
	if _, err := os.Stat(ws.Path); !os.IsNotExist(err) {
		t.Fatalf("refused binding materialized into another project: %v", err)
	}
}
