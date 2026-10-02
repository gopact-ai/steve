package turn

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
)

func sharedCopy(t *testing.T) (*Coordinator, project.Project, attempt.Record, project.Workspace) {
	t.Helper()
	c, p, source, _ := recoveryCopyFixture(t, true)
	r, err := NewAbandonControl(c).AbandonAttempt(t.Context(), source.ID, "owner", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := NewAbandonControl(c).ProjectAbandoned(t.Context(), r.ID); err != nil {
		t.Fatal(err)
	}
	workspace, err := c.workspaceFor(t.Context(), Request{ConversationID: "console:new", SenderOpenID: "owner"}, agent.Agent{ID: "worker", Node: "node", Harness: "mock"}, project.Binding{ProjectID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	if workspace.Kind != project.KindWorktree || workspace.Path == p.Home.Path {
		t.Fatal("abandonment reused the original directory instead of a shared isolated copy")
	}
	return c, p, r, workspace
}

func TestAbandonmentCopyIsSharedAndNeverReadsTheUnsettledDirectory(t *testing.T) {
	c, p, r, ws := sharedCopy(t)
	if err := os.WriteFile(filepath.Join(p.Home.Path, "original"), []byte("old writer changed this\n"), 0600); err != nil {
		t.Fatal(err)
	}
	next, err := c.workspaceFor(t.Context(), Request{ConversationID: "console:other", SenderOpenID: "owner"}, agent.Agent{ID: "another", Node: "node", Harness: "mock"}, project.Binding{ProjectID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	if next.ID != ws.ID || next.Path != ws.Path {
		t.Fatalf("second conversation got another copy: %+v %+v", ws, next)
	}
	raw, err := os.ReadFile(filepath.Join(next.Path, "original"))
	if err != nil || string(raw) != "named base\n" {
		t.Fatalf("copy read from unsettled disk: %q %v", raw, err)
	}
	episode, err := c.attempts.WorkspaceRecovery(t.Context(), r.Abandoned.WorkspaceRecoveryID)
	if err != nil || episode.Phase != "ready" || episode.Workspace.ID != ws.ID {
		t.Fatalf("copy ownership was not durable before work: %+v %v", episode, err)
	}
	if _, err := c.workspaceFor(t.Context(), Request{ConversationID: "console:elsewhere", SenderOpenID: "owner"}, agent.Agent{ID: "another", Node: "elsewhere", Harness: "mock"}, project.Binding{ProjectID: p.ID}); err == nil {
		t.Fatal("a second node silently materialized another shared copy")
	}
	if err := c.artifacts.Discard(t.Context(), ws); err == nil {
		t.Fatal("generic cleanup discarded a recovery-owned copy")
	}
}

func recoveryCopySpec(t *testing.T, c *Coordinator, p project.Project, ws project.Workspace, id string) attempt.Spec {
	t.Helper()
	tracked, err := c.tasks.Create(task.Task{Channel: "console:" + id, Transport: "console", Member: "worker", ProjectID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.tasks.Begin(tracked.ID, "worker", ws.Node, ""); err != nil {
		t.Fatal(err)
	}
	token, err := c.tasks.ExecutionToken(tracked.ID)
	if err != nil {
		t.Fatal(err)
	}
	spec, _, err := c.turnSpec(t.Context(), Request{MessageID: "web-" + id, SenderOpenID: "owner"}, agent.Agent{ID: "worker", Node: ws.Node, Harness: "mock"}, tracked.ID, project.Binding{ProjectID: p.ID}, ws)
	if err != nil {
		t.Fatal(err)
	}
	spec.ID, spec.Execution = id, &token
	return spec
}

func runCopyAttempt(t *testing.T, c *Coordinator, spec attempt.Spec) attempt.Record {
	t.Helper()
	r, err := c.attempts.Open(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.tasks.BindAttempt(*r.Execution, r.ID, r.TurnID); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []attempt.State{attempt.Prepared, attempt.Running} {
		r, err = c.attempts.Advance(t.Context(), r.ID, phase, "fixture", nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func TestSharedRecoveryCopyPinsTheLatestHeadAfterTakingItsLease(t *testing.T) {
	c, p, source, ws := sharedCopy(t)
	firstSpec := recoveryCopySpec(t, c, p, ws, "copy-first")
	queuedSpec := recoveryCopySpec(t, c, p, ws, "copy-next")
	first := runCopyAttempt(t, c, firstSpec)
	if first.WorkspaceRecovery == nil {
		t.Fatal("recovery writer has no durable episode identity")
	}
	if _, err := c.attempts.Open(t.Context(), queuedSpec); err == nil {
		t.Fatal("shared recovery copy admitted two writers")
	}
	if err := os.MkdirAll(filepath.Join(ws.Path, "inputs"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.Path, "inputs", "owned"), []byte("ordinary project content\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.Path, "next"), []byte("first output\n"), 0600); err != nil {
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
	finished, err := c.attempts.FinishCompletion(t.Context(), first.ID, "fixture", completion)
	if err != nil {
		t.Fatal(err)
	}
	episode, err := c.attempts.WorkspaceRecovery(t.Context(), source.Abandoned.WorkspaceRecoveryID)
	if err != nil {
		t.Fatal(err)
	}
	if finished.Result.Artifact == "" || episode.Head.Artifact != finished.Result.Artifact || episode.Head.Version != 2 || episode.Producer != nil {
		t.Fatalf("completion did not commit shared head: %+v %+v", finished.Result, episode)
	}
	if _, err := os.Stat(filepath.Join(ws.Path, "inputs", "owned")); err != nil {
		t.Fatal("publishing a recovery copy deleted original inputs content")
	}
	next := runCopyAttempt(t, c, queuedSpec)
	if next.Base != episode.Head.Artifact || next.Workspace.Base != next.Base || next.WorkspaceRecovery.HeadVersion != 2 {
		t.Fatalf("queued input used stale base: %+v", next.Spec)
	}
}

func TestUnpublishedRecoveryOutputBlocksReplacementEvenAfterAttemptFailure(t *testing.T) {
	c, p, source, ws := sharedCopy(t)
	first := runCopyAttempt(t, c, recoveryCopySpec(t, c, p, ws, "copy-first"))
	if err := os.WriteFile(filepath.Join(ws.Path, "result"), []byte("must survive\n"), 0600); err != nil {
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
	forceStopTrigger(t, c, `CREATE TRIGGER refuse_recovery_head BEFORE UPDATE ON operations WHEN NEW.kind='workspace-recovery' AND json_extract(NEW.data,'$.head.version')>1 BEGIN SELECT RAISE(ABORT,'head refused'); END`)
	_, cause := c.attempts.FinishCompletion(t.Context(), first.ID, "fixture", completion)
	if cause == nil {
		t.Fatal("head refusal did not reject completion")
	}
	if err := c.attempts.RejectCompletion(t.Context(), first.ID, "fixture", completion, cause); !errors.Is(err, cause) && err == nil {
		t.Fatal("rejected completion lost its error")
	}
	if _, err := c.attempts.Open(t.Context(), recoveryCopySpec(t, c, p, ws, "blocked")); !errors.Is(err, attempt.ErrWorkspaceRecovery) {
		t.Fatalf("failed attempt released unpublished recovery work: %v", err)
	}
	episode, err := c.attempts.WorkspaceRecovery(t.Context(), source.Abandoned.WorkspaceRecoveryID)
	if err != nil || episode.Producer == nil || episode.Producer.Attempt != first.ID || episode.Head.Version != 1 {
		t.Fatalf("rejected head lost the durable producer obligation: %+v %v", episode, err)
	}
	if err := ledgerOf(t, c).Update(t.Context(), func(tx *ledger.Tx) error { _, err := tx.Exec("DROP TRIGGER refuse_recovery_head"); return err }); err != nil {
		t.Fatal(err)
	}
	_, err = c.workspaceFor(t.Context(), Request{ConversationID: "console:retry", SenderOpenID: "owner"}, agent.Agent{ID: "worker", Node: "node", Harness: "mock"}, project.Binding{ProjectID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	episode, err = c.attempts.WorkspaceRecovery(t.Context(), source.Abandoned.WorkspaceRecoveryID)
	if err != nil || episode.Producer != nil || episode.Head.Artifact != completion.Result.Artifact {
		t.Fatalf("exact captured output did not replay: %+v %v", episode, err)
	}
	if raw, err := os.ReadFile(filepath.Join(ws.Path, "result")); err != nil || string(raw) != "must survive\n" {
		t.Fatal("replaying metadata overwrote working content")
	}
}

func TestRecoveryRetryUsesThePinnedArtifactRatherThanALaterCanonicalRef(t *testing.T) {
	c, p, source, base := recoveryCopyFixture(t, true)
	r, err := NewAbandonControl(c).AbandonAttempt(t.Context(), source.ID, "owner", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := NewAbandonControl(c).ProjectAbandoned(t.Context(), r.ID); err != nil {
		t.Fatal(err)
	}
	if err := ledgerOf(t, c).Update(t.Context(), func(tx *ledger.Tx) error {
		ref, _, err := tx.Name(artifact.CanonicalRef(p.ID))
		if err != nil {
			return err
		}
		_, err = tx.CompareAndSetName(ref.Name, ref.Version, strings.Repeat("9", 40))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.Home.Path, "original"), []byte("unsettled current disk"), 0600); err != nil {
		t.Fatal(err)
	}
	ws, err := c.workspaceFor(t.Context(), Request{ConversationID: "console:later", SenderOpenID: "owner"}, agent.Agent{ID: "worker", Node: "node", Harness: "mock"}, project.Binding{ProjectID: p.ID})
	if err != nil || ws.Base != base {
		t.Fatalf("retry replaced the pinned artifact with a current name: %+v %v", ws, err)
	}
	if raw, err := os.ReadFile(filepath.Join(ws.Path, "original")); err != nil || string(raw) != "named base\n" {
		t.Fatalf("retry read a later name or unsettled disk: %q %v", raw, err)
	}
}

func TestRecoveryPreparationReplaysAfterReadyCommitFailureAndSurvivesSweep(t *testing.T) {
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
	episode, err = c.attempts.SelectRecoveryWorkspace(t.Context(), episode.ID, ws, "owner")
	if err != nil {
		t.Fatal(err)
	}
	forceStopTrigger(t, c, `CREATE TRIGGER refuse_ready BEFORE UPDATE ON operations WHEN NEW.kind='workspace-recovery' AND NEW.state='ready' BEGIN SELECT RAISE(ABORT,'ready refused'); END`)
	if _, err := c.attempts.PrepareWorkspaceRecovery(t.Context(), episode, c.artifacts.PrepareRecoveryWorkspace); err == nil {
		t.Fatal("refused readiness was reported as accepted")
	}
	current, err := c.attempts.WorkspaceRecovery(t.Context(), episode.ID)
	if err != nil || current.Phase != "materializing" || current.Workspace != ws {
		t.Fatalf("failed ready commit lost materialization ownership: %+v %v", current, err)
	}
	if raw, err := os.ReadFile(filepath.Join(ws.Path, "original")); err != nil || string(raw) != "named base\n" {
		t.Fatalf("prepared fixed-base files missing: %q %v", raw, err)
	}
	container := filepath.Dir(ws.Path)
	old := time.Now().Add(-2 * artifact.SweepAge)
	if err := os.Chtimes(container, old, old); err != nil {
		t.Fatal(err)
	}
	if removed, err := c.artifacts.SweepWorktrees(t.Context(), ws.Node, filepath.Dir(filepath.Dir(container)), nil); err != nil || len(removed) != 0 {
		t.Fatalf("materialization obligation was swept: %v %v", removed, err)
	}
	if err := ledgerOf(t, c).Update(t.Context(), func(tx *ledger.Tx) error { _, err := tx.Exec("DROP TRIGGER refuse_ready"); return err }); err != nil {
		t.Fatal(err)
	}
	current, err = attempt.New(ledgerOf(t, c)).PrepareWorkspaceRecovery(t.Context(), current, c.artifacts.PrepareRecoveryWorkspace)
	if err != nil || current.Phase != "ready" || current.Workspace != ws {
		t.Fatalf("exact preparation did not replay after reopen: %+v %v", current, err)
	}
}

func setRecoveryDraining(t *testing.T, c *Coordinator, id string) {
	t.Helper()
	if err := ledgerOf(t, c).Update(t.Context(), func(tx *ledger.Tx) error {
		_, err := tx.Exec(`UPDATE operations SET state='draining',revision=revision+1,data=json_set(data,'$.phase','draining','$.revision',revision+1) WHERE kind='workspace-recovery' AND id=?`, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDrainingRecoveryKeepsItsReservedProducerAndNeverReopensTheCopy(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "unchanged", true: "changed"}[changed], func(t *testing.T) {
			c, p, source, ws := sharedCopy(t)
			writer, err := c.attempts.Open(t.Context(), recoveryCopySpec(t, c, p, ws, "draining-writer"))
			if err != nil {
				t.Fatal(err)
			}
			setRecoveryDraining(t, c, source.Abandoned.WorkspaceRecoveryID)
			if err := c.attempts.MarkRecoveryWriting(t.Context(), writer.ID); err != nil {
				t.Fatalf("draining refused its exact reserved producer before native preparation: %v", err)
			}
			for _, phase := range []attempt.State{attempt.Prepared, attempt.Running} {
				writer, err = c.attempts.Advance(t.Context(), writer.ID, phase, "fixture", nil)
				if err != nil {
					t.Fatal(err)
				}
			}
			if changed {
				if err := os.WriteFile(filepath.Join(ws.Path, "drained-output"), []byte("exact reserved output\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := c.attempts.MarkSessionSettled(t.Context(), writer.ID, "fixture"); err != nil {
				t.Fatal(err)
			}
			writer, err = c.attempts.Get(t.Context(), writer.ID)
			if err != nil {
				t.Fatal(err)
			}
			completion, _, err := c.completion(t.Context(), writer, Result{Text: "drained"}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.attempts.FinishCompletion(t.Context(), writer.ID, "fixture", completion); err != nil {
				t.Fatalf("draining lost the reserved producer's exact output: %v", err)
			}
			episode, err := c.attempts.WorkspaceRecovery(t.Context(), source.Abandoned.WorkspaceRecoveryID)
			if err != nil || episode.Phase != "draining" || episode.Producer != nil || episode.Head.Version != int64(1+len(episode.Head.Sources)) || changed && episode.Head.Artifact != completion.Result.Artifact {
				t.Fatalf("completion reopened or lost the frozen copy: %+v %v", episode, err)
			}
			if _, err := c.attempts.Open(t.Context(), recoveryCopySpec(t, c, p, ws, "new-after-drain")); !errors.Is(err, attempt.ErrWorkspaceRecovery) {
				t.Fatalf("a later input entered the draining copy: %v", err)
			}
		})
	}
}

func TestDrainingRecoveryReplaysOnlyItsExactRejectedOutput(t *testing.T) {
	c, p, source, ws := sharedCopy(t)
	writer := runCopyAttempt(t, c, recoveryCopySpec(t, c, p, ws, "draining-replay"))
	if err := os.WriteFile(filepath.Join(ws.Path, "drained-output"), []byte("accepted candidate\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := c.attempts.MarkSessionSettled(t.Context(), writer.ID, "fixture"); err != nil {
		t.Fatal(err)
	}
	writer, err := c.attempts.Get(t.Context(), writer.ID)
	if err != nil {
		t.Fatal(err)
	}
	completion, _, err := c.completion(t.Context(), writer, Result{Text: "drained"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	forceStopTrigger(t, c, `CREATE TRIGGER refuse_draining_head BEFORE UPDATE ON operations WHEN NEW.kind='workspace-recovery' AND json_extract(NEW.data,'$.head.version')>1 BEGIN SELECT RAISE(ABORT,'draining head refused'); END`)
	_, cause := c.attempts.FinishCompletion(t.Context(), writer.ID, "fixture", completion)
	if cause == nil {
		t.Fatal("head refusal did not reject completion")
	}
	if err := c.attempts.RejectCompletion(t.Context(), writer.ID, "fixture", completion, cause); err == nil {
		t.Fatal("rejected candidate lost its failure")
	}
	if err := ledgerOf(t, c).Update(t.Context(), func(tx *ledger.Tx) error { _, err := tx.Exec("DROP TRIGGER refuse_draining_head"); return err }); err != nil {
		t.Fatal(err)
	}
	setRecoveryDraining(t, c, source.Abandoned.WorkspaceRecoveryID)
	if err := os.WriteFile(filepath.Join(ws.Path, "drained-output"), []byte("later unaccepted disk\n"), 0600); err != nil {
		t.Fatal(err)
	}
	episode, err := c.attempts.PublishRecoveryHead(t.Context(), source.Abandoned.WorkspaceRecoveryID, c.artifacts.RecoveryOutputTx)
	if err != nil || episode.Phase != "draining" || episode.Producer != nil || episode.Head.Artifact != completion.Result.Artifact {
		t.Fatalf("draining did not replay the exact candidate without reopening: %+v %v", episode, err)
	}
	if raw, err := os.ReadFile(filepath.Join(ws.Path, "drained-output")); err != nil || string(raw) != "later unaccepted disk\n" {
		t.Fatalf("replaying metadata replaced later disk content: %q %v", raw, err)
	}
}

func TestDrainingRecoveryReleasesOnlyAnExactlyUnusedReservation(t *testing.T) {
	c, p, source, ws := sharedCopy(t)
	writer, err := c.attempts.Open(t.Context(), recoveryCopySpec(t, c, p, ws, "draining-unused"))
	if err != nil {
		t.Fatal(err)
	}
	setRecoveryDraining(t, c, source.Abandoned.WorkspaceRecoveryID)
	if _, err := c.attempts.FailWith(t.Context(), writer.ID, "fixture", "unused reservation", nil); err != nil {
		t.Fatal(err)
	}
	episode, err := c.attempts.PublishRecoveryHead(t.Context(), source.Abandoned.WorkspaceRecoveryID, c.artifacts.RecoveryOutputTx)
	if err != nil || episode.Phase != "draining" || episode.Producer != nil || episode.Head.Version != 1 {
		t.Fatalf("unused reservation reopened the copy or changed its accepted head: %+v %v", episode, err)
	}
}

func TestRecoveryUnchangedNativeContextRetainsTaskAndRequiresExactMachineExit(t *testing.T) {
	c, p, source, ws := sharedCopy(t)
	writer, err := c.attempts.Open(t.Context(), recoveryCopySpec(t, c, p, ws, "idle-copy"))
	if err != nil {
		t.Fatal(err)
	}
	writer, err = c.attempts.Advance(t.Context(), writer.ID, attempt.Prepared, "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	writer, err = c.attempts.RecordSession(t.Context(), writer.ID, "fixture", "ns_idle_copy")
	if err != nil {
		t.Fatal(err)
	}
	writer, err = c.attempts.Advance(t.Context(), writer.ID, attempt.Running, "fixture", func(r *attempt.Record) { r.NativeContext = "native-idle-copy" })
	if err != nil {
		t.Fatal(err)
	}
	session := state.Session{ConversationID: "console:idle-copy", AgentID: writer.Agent, HarnessID: writer.Harness, NodeID: writer.Node, UpstreamID: writer.Session, Workspace: ws.Path, ProjectID: p.ID}
	if err := c.store.SaveSession(session); err != nil {
		t.Fatal(err)
	}
	if err := c.attempts.MarkSessionSettled(t.Context(), writer.ID, "fixture"); err != nil {
		t.Fatal(err)
	}
	writer, err = c.attempts.Get(t.Context(), writer.ID)
	if err != nil {
		t.Fatal(err)
	}
	completion, _, err := c.completion(t.Context(), writer, Result{Text: "unchanged"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.attempts.FinishCompletion(t.Context(), writer.ID, "fixture", completion); err != nil {
		t.Fatal(err)
	}
	episode, err := c.attempts.WorkspaceRecovery(t.Context(), source.Abandoned.WorkspaceRecoveryID)
	if err != nil || episode.Head.Version != 1 || len(episode.Head.Sources) != 0 || len(episode.NativeRetirements) != 1 {
		t.Fatalf("unchanged turn lost native cleanup ownership: %+v %v", episode, err)
	}
	if err := ledgerOf(t, c).Read(t.Context(), func(tx *ledger.ReadTx) error { return attempt.CheckTaskDeletionTx(tx, []string{writer.TaskID}) }); !errors.Is(err, task.ErrRetirementPending) {
		t.Fatalf("unchanged task authority was deletable before native retirement: %v", err)
	}
	if err := c.attempts.DriveWorkspaceRecovery(t.Context(), episode.ID, func(ctx context.Context, driver ledger.Lease) error {
		native := episode.NativeRetirements[0]
		proof := attempt.RetainedEvidence{ObservedAt: time.Now().UTC(), Session: nodewire.SessionState{ID: native.Session, ContextID: native.Context, Harness: native.Harness, Binding: native.Binding, State: nodewire.SessionClosed}}
		if err := c.attempts.AcceptRecoveryNativeStop(ctx, episode, native, driver, proof); !errors.Is(err, attempt.ErrStopConfirmationRequired) {
			t.Fatalf("command settlement became machine proof: %v", err)
		}
		proof.Session.ProcessStopped = true
		proof.Session.Binding.AttemptID = "another-binding"
		if err := c.attempts.AcceptRecoveryNativeStop(ctx, episode, native, driver, proof); !errors.Is(err, attempt.ErrStopConfirmationRequired) {
			t.Fatalf("wrong binding became machine proof: %v", err)
		}
		proof.Session.Binding = native.Binding
		forceStopTrigger(t, c, `CREATE TRIGGER refuse_native_proof BEFORE UPDATE ON operations WHEN NEW.kind='workspace-recovery' AND json_extract(NEW.data,'$.native_retirements[0].proof') IS NOT NULL BEGIN SELECT RAISE(ABORT,'native proof refused'); END`)
		if err := c.attempts.AcceptRecoveryNativeStop(ctx, episode, native, driver, proof); err == nil {
			t.Fatal("proof refusal was ignored")
		}
		current, err := c.attempts.WorkspaceRecovery(ctx, episode.ID)
		if err != nil || current.NativeRetirements[0].Proof != nil {
			t.Fatalf("rejected machine proof was accepted: %+v %v", current, err)
		}
		if err := ledgerOf(t, c).Update(ctx, func(tx *ledger.Tx) error { _, err := tx.Exec("DROP TRIGGER refuse_native_proof"); return err }); err != nil {
			return err
		}
		if err := c.store.RetireRecoverySession(ctx, native.Binding.NodeID, native.Harness, native.Session, native.Binding.AttemptID, time.Now().UTC().Format(time.RFC3339Nano), func(tx *ledger.Tx) error { return c.attempts.RetireRecoveryNativeTx(tx, episode.ID, native, driver) }); err != nil {
			return err
		}
		if err := c.store.SaveSession(session); !errors.Is(err, state.ErrAbandonedContext) {
			t.Fatalf("late save revived a retired copy cwd: %v", err)
		}
		if _, err := c.store.RestoreSession(session.ConversationID, session.AgentID, 1); !errors.Is(err, state.ErrAbandonedContext) {
			t.Fatalf("archive revived a retired copy cwd: %v", err)
		}
		return c.attempts.AcceptRecoveryNativeStop(ctx, episode, native, driver, proof)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryDrainWaitsForEveryOriginalAndEveryPhysicalWriter(t *testing.T) {
	for _, bothSources := range []bool{false, true} {
		t.Run(map[bool]string{false: "other physical writer", true: "two original sources"}[bothSources], func(t *testing.T) {
			c, first, second, episode := twoOriginalRecoverySources(t, bothSources)
			if err := NewAbandonControl(c).ProjectAbandoned(t.Context(), first.ID); err != nil {
				t.Fatal(err)
			}
			first = retireOriginalRecoverySource(t, c, first)
			if err := c.attempts.DriveWorkspaceRecovery(t.Context(), episode.ID, func(ctx context.Context, driver ledger.Lease) error {
				current, err := c.attempts.EnrollRecoveryNatives(ctx, episode.ID, false, driver)
				if err != nil {
					return err
				}
				for _, native := range current.NativeRetirements {
					if native.Binding.AttemptID != first.ID {
						continue
					}
					if _, err := c.attempts.AdoptRecoveryNativeStop(ctx, episode.ID, native, driver); err != nil {
						return err
					}
					if err := c.store.RetireRecoverySession(ctx, native.Binding.NodeID, native.Harness, native.Session, native.Binding.AttemptID, time.Now().UTC().Format(time.RFC3339Nano), func(tx *ledger.Tx) error { return c.attempts.RetireRecoveryNativeTx(tx, episode.ID, native, driver) }); err != nil {
						return err
					}
				}
				if _, err := c.attempts.BeginRecoveryDrain(ctx, episode.ID, driver); err == nil {
					t.Fatalf("drain forgot unstopped physical writer %s", second.ID)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			current, err := c.attempts.WorkspaceRecovery(t.Context(), episode.ID)
			if err != nil || current.Phase != "recorded" {
				t.Fatalf("refused drain changed phase: %+v %v", current, err)
			}
		})
	}
}

func enrollStoppedOriginals(t *testing.T, ctx context.Context, c *Coordinator, id string, driver ledger.Lease) attempt.WorkspaceRecovery {
	t.Helper()
	episode, err := c.attempts.EnrollRecoveryNatives(ctx, id, false, driver)
	if err != nil {
		t.Fatal(err)
	}
	for _, native := range episode.NativeRetirements {
		if native.Copy {
			continue
		}
		adopted, err := c.attempts.AdoptRecoveryNativeStop(ctx, id, native, driver)
		if err != nil || !adopted {
			t.Fatalf("original native proof: %v %v", adopted, err)
		}
		if err := c.store.RetireRecoverySession(ctx, native.Binding.NodeID, native.Harness, native.Session, native.Binding.AttemptID, time.Now().UTC().Format(time.RFC3339Nano), func(tx *ledger.Tx) error { return c.attempts.RetireRecoveryNativeTx(tx, id, native, driver) }); err != nil {
			t.Fatal(err)
		}
	}
	episode, err = c.attempts.WorkspaceRecovery(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return episode
}

func TestRecoveryDrainSerializesBothOrdersWithProducerReservation(t *testing.T) {
	for _, reserveFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "drain first", true: "reservation first"}[reserveFirst], func(t *testing.T) {
			c, p, source, ws := sharedCopy(t)
			source = retireOriginalRecoverySource(t, c, source)
			spec := recoveryCopySpec(t, c, p, ws, "reserved-at-boundary")
			if err := c.attempts.DriveWorkspaceRecovery(t.Context(), source.Abandoned.WorkspaceRecoveryID, func(ctx context.Context, driver ledger.Lease) error {
				episode := enrollStoppedOriginals(t, ctx, c, source.Abandoned.WorkspaceRecoveryID, driver)
				var writer attempt.Record
				var err error
				if reserveFirst {
					writer, err = c.attempts.Open(ctx, spec)
					if err != nil {
						return err
					}
				}
				drained, err := c.attempts.BeginRecoveryDrain(ctx, episode.ID, driver)
				if err != nil {
					return err
				}
				if drained.Phase != "draining" || (drained.Producer != nil) != reserveFirst {
					t.Fatalf("drain discarded prior reservation: %+v", drained)
				}
				if !reserveFirst {
					if _, err := c.attempts.Open(ctx, spec); !errors.Is(err, attempt.ErrWorkspaceRecovery) {
						t.Fatalf("reservation beat committed drain: %v", err)
					}
					return nil
				}
				if _, err := c.attempts.FailWith(ctx, writer.ID, "fixture", "unused at drain boundary", nil); err != nil {
					return err
				}
				drained, err = c.attempts.PublishRecoveryHead(ctx, episode.ID, c.artifacts.RecoveryOutputTx)
				if err != nil || drained.Phase != "draining" || drained.Producer != nil {
					t.Fatalf("unused boundary reopened copy: %+v %v", drained, err)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
