package coordination

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func addUnjoinedTestReplica(t *testing.T, cluster *testCluster, domain string, levels ...string) *Service {
	t.Helper()
	cfg := cluster.configs["node-1"]
	cfg.NodeID = "new-node"
	cfg.FailureDomain = domain
	if len(levels) > 0 {
		cfg.StorageLevel = levels[0]
	}
	cfg.DataDir = t.TempDir()
	cfg.BindAddress = "127.0.0.1:0"
	cfg.Bootstrap = false
	if cfg.Application != nil {
		cfg.Application = openCounter(t, cfg.DataDir)
	}
	peer, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cluster.mu.Lock()
	cluster.nodes[cfg.NodeID] = peer
	cluster.mu.Unlock()
	cluster.configs[cfg.NodeID] = cfg
	return peer
}

func TestUnauthorizedStorageLevelReceivesNoApplicationBaseline(t *testing.T) {
	for _, level := range []string{"public", "internal", ""} {
		t.Run("level-"+level, func(t *testing.T) {
			cluster := newTestCluster(t, 1, func(_ string, dir string) Application { return openCounter(t, dir) })
			leader := cluster.leader()
			source := cluster.configs["node-1"].Application.(*durableCounter)
			source.mu.Lock()
			source.count = 71
			if err := source.persist(); err != nil {
				t.Fatal(err)
			}
			source.mu.Unlock()
			peer := addUnjoinedTestReplica(t, cluster, "low-trust-domain", level)
			_, err := leader.Join(t.Context(), JoinRequest{ID: "low-trust-join", Actor: "owner", Member: Member{NodeID: "new-node", Address: peer.Status().Address}})
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("low-trust full replica accepted: %v", err)
			}
			if len(leader.TransportPeers()) != 1 || len(leader.Status().Members) != 1 {
				t.Fatal("unauthorized replica entered membership before rejection")
			}
			if value := cluster.configs["new-node"].Application.(*durableCounter).value(); value != 0 {
				t.Fatalf("unauthorized replica received private baseline: %d", value)
			}
		})
	}
}

func TestReplicaPolicyRunsBeforeMembershipOrSnapshot(t *testing.T) {
	cluster := newTestCluster(t, 1, func(_ string, dir string) Application { return openCounter(t, dir) })
	leader := cluster.leader()
	peer := addUnjoinedTestReplica(t, cluster, "policy-domain")
	var checked atomic.Bool
	leader.config.AuthorizeReplica = func(context.Context, Member) error {
		checked.Store(true)
		return errors.New("shared ledger contains excluded data")
	}
	_, err := leader.Join(t.Context(), JoinRequest{ID: "policy-rejected", Actor: "owner", Member: Member{NodeID: "new-node", Address: peer.Status().Address}})
	if !checked.Load() || !errors.Is(err, ErrInvalid) {
		t.Fatalf("replica policy was not enforced: %v", err)
	}
	if len(leader.TransportPeers()) != 1 || len(leader.Status().Members) != 1 {
		t.Fatal("replica policy ran after candidate admission")
	}
}

func TestApplicationWritesContinueWhileJoiningPeerVerifiesNetwork(t *testing.T) {
	cluster := newTestCluster(t, 1, func(_ string, dir string) Application { return openCounter(t, dir) })
	leader := cluster.leader()
	peer := addUnjoinedTestReplica(t, cluster, "another-machine")
	verifying := make(chan struct{})
	release := make(chan struct{})
	leader.config.ValidateJoin = func(ctx context.Context, _ Member) error {
		close(verifying)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
			return nil
		}
	}
	joined := make(chan error, 1)
	go func() {
		_, err := leader.Join(t.Context(), JoinRequest{ID: "slow-network-check", Actor: "owner", Member: Member{NodeID: "new-node", Address: peer.Status().Address}})
		joined <- err
	}()
	select {
	case <-verifying:
	case <-time.After(3 * time.Second):
		t.Fatal("candidate did not reach network verification")
	}
	written := make(chan error, 1)
	go func() {
		_, err := leader.ApplyApp(t.Context(), AppCommand{ID: "work-during-enrollment", CallerNodeID: "node-1", CoordinatorEpoch: 1, WriterGeneration: 1, Payload: []byte("1")})
		written <- err
	}()
	var writeErr error
	select {
	case writeErr = <-written:
	case <-time.After(time.Second):
		writeErr = errors.New("application write blocked behind candidate network verification")
	}
	close(release)
	if err := <-joined; err != nil {
		t.Fatal(err)
	}
	if writeErr != nil {
		t.Fatal(writeErr)
	}
}

func TestJoiningReplicaCannotVoteBeforeNetworkValidation(t *testing.T) {
	cluster := newTestCluster(t, 1)
	leader := cluster.leader()
	peer := addUnjoinedTestReplica(t, cluster, "independent-test-domain")
	var reachable atomic.Bool
	leader.config.ValidateJoin = func(context.Context, Member) error {
		if !reachable.Load() {
			return errors.New("candidate cannot reach another member")
		}
		return nil
	}
	request := JoinRequest{ID: "mesh-checked-join", Actor: "owner", Member: Member{NodeID: "new-node", Address: peer.Status().Address}}
	if _, err := leader.Join(context.Background(), request); !errors.Is(err, ErrNotReady) {
		t.Fatalf("network validation failure was hidden: %v", err)
	}
	if len(leader.Status().Voters) != 1 || leader.TransportPeers()["new-node"] == "" {
		t.Fatal("unverified candidate did not remain a nonvoter")
	}
	if _, err := leader.Transfer(context.Background(), TransferRequest{ID: "unready-transfer", Actor: "owner", ExpectedEpoch: 1, TargetNodeID: "new-node"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unverified candidate became coordinator: %v", err)
	}
	reachable.Store(true)
	if _, err := leader.Join(context.Background(), request); err != nil {
		t.Fatalf("same operation could not resume after connectivity recovered: %v", err)
	}
	if len(leader.Status().Voters) != 2 {
		t.Fatal("verified candidate was not promoted")
	}
	joins := 0
	for _, event := range leader.Status().Audit {
		if event.CommandID == request.ID {
			joins++
		}
	}
	if joins != 1 {
		t.Fatalf("join retry duplicated audit: %d", joins)
	}
}

func TestDifferentNodeIDsOnOnePhysicalMachineCannotBecomeTwoVoters(t *testing.T) {
	cluster := newTestCluster(t, 1)
	leader := cluster.leader()
	peer := addUnjoinedTestReplica(t, cluster, leader.Status().FailureDomain)
	_, err := leader.Join(context.Background(), JoinRequest{ID: "same-machine", Actor: "owner", Member: Member{NodeID: "new-node", Address: peer.Status().Address}})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("duplicate physical failure domain was accepted: %v", err)
	}
	if len(leader.TransportPeers()) != 1 || len(leader.Status().Members) != 1 {
		t.Fatal("duplicate physical machine changed consensus membership")
	}
}

func TestApplicationProgressDoesNotInvalidateReviewedControlPolicy(t *testing.T) {
	cluster := newTestCluster(t, 1, func(_ string, dir string) Application { return openCounter(t, dir) })
	leader := cluster.leader()
	state, err := leader.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leader.ApplyApp(t.Context(), AppCommand{ID: "task-progress", CallerNodeID: "node-1", CoordinatorEpoch: 1, WriterGeneration: 1, Payload: []byte("1")}); err != nil {
		t.Fatal(err)
	}
	if _, err := leader.SetEligibility(t.Context(), EligibilityRequest{ID: "reviewed-policy", Actor: "owner", ExpectedRevision: state.Revision, NodeID: "node-1", Eligible: false}); err != nil {
		t.Fatalf("unrelated task progress invalidated control review: %v", err)
	}
}
