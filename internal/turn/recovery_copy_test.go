package turn

import (
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
	"github.com/gopact-ai/steve/internal/project"
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
