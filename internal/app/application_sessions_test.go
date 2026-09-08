package app

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/cluster"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

func TestCommittedSessionCleanupUsesNativeIdentityWithoutWorkdir(t *testing.T) {
	record := attempt.Record{Spec: attempt.Spec{Node: "node-a", Harness: "test", Workspace: project.Workspace{Path: "/fixture/worktree"}}, Session: "ns_original"}
	place := harness.Placement{Node: "node-a", Harness: "test"}
	if err := validateSessionPlacement(record, place, "ns_original", ""); err != nil {
		t.Fatalf("committed native cleanup refused: %v", err)
	}
	for _, id := range []string{"ns_other", "raw-session"} {
		if err := validateSessionPlacement(record, place, id, ""); err == nil {
			t.Fatalf("empty workdir accepted for unrelated native identity %q", id)
		}
	}
	if err := validateSessionPlacement(record, place, "", ""); err != nil {
		t.Fatalf("admitted capability inquiry refused: %v", err)
	}
	if err := validateSessionPlacement(record, place, "ns_original", "/another/worktree"); err == nil {
		t.Fatal("mismatched workspace accepted")
	}
	if err := validateSessionPlacement(record, harness.Placement{Node: "node-b", Harness: "test"}, "ns_original", ""); err == nil {
		t.Fatal("cleanup accepted on wrong physical machine")
	}
	if err := validateSessionPlacement(record, place, "", "/fixture/worktree"); err != nil {
		t.Fatalf("admitted new session refused: %v", err)
	}
}

func TestExecutorSessionAuthorityUsesCommittedExecutionAndActiveCoordinator(t *testing.T) {
	options, _ := testPeerOptions(t, ClusterPeerTestDir(t), nil)
	activated := make(chan cluster.Activation, 1)
	var count atomic.Int32
	application := testPeerApplication(t, &count)
	options.Activate = func(ctx context.Context, active cluster.Activation, ready func(cluster.PeerApplicationEndpoint) error) (cluster.Deactivate, error) {
		stop, err := application(ctx, active, ready)
		activated <- active
		return stop, err
	}
	peer := StartTestPeer(t, options)
	var active cluster.Activation
	select {
	case active = <-activated:
	case <-time.After(10 * time.Second):
		t.Fatal("coordinator did not activate")
	}
	tasks, err := task.OpenLedger(active.Ledger, "")
	if err != nil {
		t.Fatal(err)
	}
	tracked, err := tasks.Create(task.Task{Goal: "executor session", Channel: "console:executor", Member: "mock", ProjectID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Begin(tracked.ID, "mock", "worker", ""); err != nil {
		t.Fatal(err)
	}
	token, err := tasks.ExecutionToken(tracked.ID)
	if err != nil {
		t.Fatal(err)
	}
	attempts := attempt.New(active.Ledger)
	record, err := attempts.Open(t.Context(), attempt.Spec{ID: "executor-attempt", TaskID: tracked.ID, TurnID: "input-1", Kind: attempt.KindChat, Project: "p", Node: "worker", Harness: "mock", Agent: "mock", Execution: &token, Scope: attempt.ScopePathSet, Workspace: project.Workspace{ID: "workspace-1", Project: "p", Node: "worker", Path: t.TempDir(), Kind: project.KindWorktree}})
	if err != nil {
		t.Fatal(err)
	}
	if err := tasks.BindAttempt(token, record.ID, record.TurnID); err != nil {
		t.Fatal(err)
	}
	if _, err := attempts.Advance(t.Context(), record.ID, attempt.Prepared, "test", nil); err != nil {
		t.Fatal(err)
	}
	contextWithCancel, cancel := context.WithCancel(active.Context)
	defer cancel()
	active.Context = contextWithCancel
	verify := peer.ApplicationSessionAuthorizer(active)
	a := nodewire.SessionAuthority{ClusterID: peer.Config.ClusterID, CoordinatorNodeID: active.NodeID, CoordinatorEpoch: active.Assignment.Epoch, WriterGeneration: active.WriterGeneration}
	b := nodewire.SessionBinding{ProjectID: "p", SessionID: cluster.LogicalAgentSession(tracked.Channel, tracked.ID, record.Agent), TaskID: tracked.ID, AttemptID: record.ID, NodeID: "worker", ExecutionEpoch: attempt.SessionExecutionEpoch(record), TaskEpoch: token.Epoch}
	if err := verify(t.Context(), "worker", a, b, "start"); err != nil {
		t.Fatalf("committed executor preparation refused: %v", err)
	}
	if err := verify(t.Context(), "other-worker", a, b, "start"); err == nil {
		t.Fatal("worker connection could authorize another node")
	}
	if err := verify(t.Context(), "worker", a, b, "prompt"); err == nil {
		t.Fatal("prepared execution could dispatch before running commit")
	}
	wrong := a
	wrong.CoordinatorEpoch++
	if err := verify(t.Context(), "worker", wrong, b, "start"); err == nil {
		t.Fatal("uncommitted coordinator epoch accepted")
	}
	wrongBinding := b
	wrongBinding.TaskEpoch++
	if err := verify(t.Context(), "worker", a, wrongBinding, "start"); err == nil {
		t.Fatal("uncommitted task epoch accepted")
	}
	if _, err := attempts.Advance(t.Context(), record.ID, attempt.Running, "test", nil); err != nil {
		t.Fatal(err)
	}
	if err := verify(t.Context(), "worker", a, b, "prompt"); err != nil {
		t.Fatalf("committed running executor refused: %v", err)
	}
	cancel()
	if err := verify(t.Context(), "worker", a, b, "attach"); err == nil {
		t.Fatal("inactive coordinator could observe executor session")
	}
}
