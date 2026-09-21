package coordination

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/hashicorp/raft"
)

func joinNonvoter(t *testing.T, c *testCluster) (*Service, *Service) {
	t.Helper()
	leader := c.leader()
	peer := addUnjoinedTestReplica(t, c, "nonvoter-domain")
	if _, err := leader.Join(t.Context(), JoinRequest{ID: "join-nonvoter", Actor: "owner", Member: Member{NodeID: "new-node", Address: peer.Status().Address}}); err != nil {
		t.Fatal(err)
	}
	return leader, peer
}

func TestManualNonvoterTransferPreservesFencesAndDeduplication(t *testing.T) {
	c := newTestCluster(t, 1, func(_ string, dir string) Application { return openCounter(t, dir) })
	leader, _ := joinNonvoter(t, c)
	before := leader.Status()
	request := TransferRequest{ID: "manual-nonvoter", Actor: "owner", ExpectedEpoch: 1, TargetNodeID: "new-node"}
	result, err := leader.Transfer(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := leader.Transfer(t.Context(), request)
	if err != nil || !reflect.DeepEqual(result, replay) {
		t.Fatalf("retry changed result: %+v %v", replay, err)
	}
	state := leader.Status()
	if len(state.Voters) != 1 || state.Members["new-node"].Voting || state.AutoFailover || state.ConfigurationIndex != before.ConfigurationIndex {
		t.Fatal("manual assignment changed consensus or policy")
	}
	if _, err := leader.ApplyApp(t.Context(), AppCommand{ID: "stale-hub", CallerNodeID: "node-1", CoordinatorEpoch: 1, WriterGeneration: 1, Payload: []byte("100")}); !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("old hub was not fenced: %v", err)
	}
	fence, err := leader.BeginWriter(t.Context(), WriterRequest{ID: "nonvoter-writer", CallerNodeID: "new-node", CoordinatorEpoch: 2, ExpectedGeneration: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leader.ApplyApp(t.Context(), AppCommand{ID: "new-hub-write", CallerNodeID: "new-node", CoordinatorEpoch: 2, WriterGeneration: fence.WriterGeneration, Payload: []byte("3")}); err != nil {
		t.Fatal(err)
	}
}

func TestNonvoterTransferRequiresProgress(t *testing.T) {
	c := newTestCluster(t, 1)
	leader, _ := joinNonvoter(t, c)
	leader.config.Probe = func(context.Context, Member) (Progress, error) {
		return Progress{ClusterID: "test-cluster", NodeID: "new-node", FailureDomain: "nonvoter-domain", StorageLevel: "restricted"}, nil
	}
	_, err := leader.Transfer(t.Context(), TransferRequest{ID: "lagging", Actor: "owner", ExpectedEpoch: 1, TargetNodeID: "new-node"})
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("lagging nonvoter transfer: %v", err)
	}
}

func TestNonvoterHubCannotEnableAutomaticPolicy(t *testing.T) {
	c := newTestCluster(t, 3)
	leader, _ := joinNonvoter(t, c)
	for _, id := range []string{"node-1", "node-2"} {
		if _, err := leader.SetEligibility(t.Context(), EligibilityRequest{ID: "eligible-" + id, Actor: "owner", ExpectedRevision: leader.Status().Revision, NodeID: id, Eligible: true}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := leader.Transfer(t.Context(), TransferRequest{ID: "manual", Actor: "owner", ExpectedEpoch: 1, TargetNodeID: "new-node"}); err != nil {
		t.Fatal(err)
	}
	if _, err := leader.SetAutoFailover(t.Context(), PolicyRequest{ID: "enable", Actor: "owner", ExpectedRevision: leader.Status().Revision, Enabled: true}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("enabled auto with nonvoter hub: %v", err)
	}
	if leader.Status().AutoFailover {
		t.Fatal("rejected policy changed state")
	}
}

func TestTransferFSMRechecksPolicyMembershipAndCAS(t *testing.T) {
	base := State{Coordinator: Assignment{NodeID: "hub", Epoch: 1}, ConfigurationIndex: 7, AppVersion: 3, Members: map[string]Member{"target": {NodeID: "target", Address: "addr"}}, Replicas: map[string]string{"target": "addr"}, Voters: map[string]string{}, Removing: map[string]bool{}, PendingJoins: map[string]bool{}, PendingVotes: map[string]VotingRequest{}}
	for _, tc := range []struct {
		name   string
		change func(*State, *command)
		want   string
	}{
		{"manual", func(*State, *command) {}, ""},
		{"policy-enabled-before-commit", func(s *State, _ *command) { s.AutoFailover = true }, "invalid"},
		{"automatic", func(_ *State, c *command) { c.Automatic = true }, "invalid"},
		{"not-replicating", func(s *State, _ *command) { delete(s.Replicas, "target") }, "invalid"},
		{"not-member", func(s *State, _ *command) { delete(s.Members, "target") }, "invalid"},
		{"removing", func(s *State, _ *command) { s.Removing["target"] = true }, "invalid"},
		{"join-not-complete", func(s *State, _ *command) { s.PendingJoins["target"] = true }, "invalid"},
		{"vote-not-complete", func(s *State, _ *command) { s.PendingVotes["target"] = VotingRequest{ID: "pending"} }, "conflict"},
		{"config-changed", func(s *State, _ *command) { s.ConfigurationIndex++ }, "conflict"},
		{"app-changed", func(s *State, _ *command) { s.AppVersion++ }, "conflict"},
		{"epoch-changed", func(s *State, _ *command) { s.Coordinator.Epoch++ }, "epoch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := cloneState(base)
			c := command{Transfer: TransferRequest{ExpectedEpoch: 1, TargetNodeID: "target"}, ExpectedAppVersion: 3, ExpectedConfigurationIndex: 7}
			tc.change(&s, &c)
			r := receipt{}
			applyTransfer(&s, c, &r)
			if r.Code != tc.want {
				t.Fatalf("receipt %+v want %s", r, tc.want)
			}
		})
	}
}

func TestVotingGrantChangesConfigurationOnceAndIsIdempotent(t *testing.T) {
	c := newTestCluster(t, 1)
	leader, _ := joinNonvoter(t, c)
	before := leader.Status()
	request := VotingRequest{ID: "grant", Actor: "owner", ExpectedRevision: before.Revision, NodeID: "new-node", Voting: true}
	result, err := leader.SetVoting(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	changes := 0
	for index := before.ConfigurationIndex + 1; index <= leader.raft.LastIndex(); index++ {
		var log raft.Log
		if err := leader.store.GetLog(index, &log); err == nil && log.Type == raft.LogConfiguration {
			changes++
		}
	}
	if changes != 1 {
		t.Fatalf("one vote grant appended %d configurations", changes)
	}
	replay, err := leader.SetVoting(t.Context(), request)
	if err != nil || !reflect.DeepEqual(result, replay) {
		t.Fatalf("vote retry: %+v %v", replay, err)
	}
	// A stale background observation must not revoke the just-granted vote.
	stale := leader.Status().State
	member := stale.Members["new-node"]
	member.Voting = false
	stale.Members["new-node"] = member
	leader.demoteNonVoters(stale)
	time.Sleep(50 * time.Millisecond)
	if leader.Status().Voters["new-node"] == "" || !leader.Status().Members["new-node"].Voting {
		t.Fatal("stale reconciliation revoked a granted vote")
	}
}

func TestVotingRPCIsOwnerAuthorizedAndRoutesThroughFollower(t *testing.T) {
	c := newTLSTestCluster(t, 3)
	client := c.clients["node-3"]
	state, err := client.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.SetVoting(t.Context(), VotingRequest{ID: "rpc-revoke", Actor: "spoofed", ExpectedRevision: state.Revision, NodeID: "node-2", Voting: false})
	if err != nil {
		t.Fatal(err)
	}
	state, err = client.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if state.Members["node-2"].Voting || state.Voters["node-2"] != "" {
		t.Fatal("RPC did not revoke vote")
	}
	last := state.Audit[len(state.Audit)-1]
	if last.Actor != "owner:test-user" || last.Index != result.Index {
		t.Fatalf("untrusted actor reached audit: %+v", last)
	}
	if _, err := client.SetVoting(t.Context(), VotingRequest{ID: "rpc-grant", ExpectedRevision: state.Revision, NodeID: "node-2", Voting: true}); err != nil {
		t.Fatal(err)
	}
}

func TestVotingRetryAfterConfigurationCommitDoesNotChangeConfigurationAgain(t *testing.T) {
	c := newTestCluster(t, 1)
	leader, _ := joinNonvoter(t, c)
	state := leader.Status()
	request := VotingRequest{ID: "interrupted-grant", Actor: "owner", ExpectedRevision: state.Revision, NodeID: "new-node", Voting: true}
	fp := fingerprint("voting", request)
	leader.membershipMu.Lock()
	_, err := leader.submit(t.Context(), command{Kind: "voting_prepare", ID: request.ID + "/prepare", Actor: request.Actor, Fingerprint: fp, Voting: request})
	if err != nil {
		leader.membershipMu.Unlock()
		t.Fatal(err)
	}
	err = leader.wait(t.Context(), leader.raft.AddVoter(raft.ServerID(request.NodeID), raft.ServerAddress(state.Members[request.NodeID].Address), state.ConfigurationIndex, leader.config.ApplyTimeout))
	if err == nil {
		err = leader.barrier(t.Context())
	}
	index := leader.Status().ConfigurationIndex
	leader.membershipMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	// Reconciliation must leave a durable, unfinished grant alone.
	leader.demoteNonVoters(leader.Status().State)
	if _, err := leader.SetVoting(t.Context(), request); err != nil {
		t.Fatalf("prepared grant did not resume: %v", err)
	}
	if got := leader.Status(); got.ConfigurationIndex != index || !got.Members[request.NodeID].Voting {
		t.Fatalf("retry altered configuration or lost vote: index %d -> %d", index, got.ConfigurationIndex)
	}
}

func TestVotingVerificationDoesNotRaceCoordinatorTransfer(t *testing.T) {
	c := newTestCluster(t, 1)
	leader, _ := joinNonvoter(t, c)
	entered, release := make(chan struct{}), make(chan struct{})
	leader.config.ValidateJoin = func(ctx context.Context, _ Member) error {
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	request := VotingRequest{ID: "grant-during-transfer", Actor: "owner", ExpectedRevision: leader.Status().Revision, NodeID: "new-node", Voting: true}
	finished := make(chan error, 1)
	go func() { _, err := leader.SetVoting(t.Context(), request); finished <- err }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("network validation did not start")
	}
	_, err := leader.Transfer(t.Context(), TransferRequest{ID: "transfer-during-vote", Actor: "owner", ExpectedEpoch: 1, TargetNodeID: "new-node"})
	close(release)
	voteErr := <-finished
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("transfer raced unfinished vote: %v", err)
	}
	if voteErr != nil {
		t.Fatal(voteErr)
	}
}

func TestNonvoterCannotBeTransferredUnderAutomaticPolicy(t *testing.T) {
	c := newTestCluster(t, 3)
	leader, _ := joinNonvoter(t, c)
	for _, id := range []string{"node-1", "node-2"} {
		if _, err := leader.SetEligibility(t.Context(), EligibilityRequest{ID: "eligible-" + id, Actor: "owner", ExpectedRevision: leader.Status().Revision, NodeID: id, Eligible: true}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := leader.SetAutoFailover(t.Context(), PolicyRequest{ID: "enable", Actor: "owner", ExpectedRevision: leader.Status().Revision, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	for _, automatic := range []bool{false, true} {
		_, err := leader.transfer(t.Context(), TransferRequest{ID: "nonvoter-with-auto", Actor: "owner", ExpectedEpoch: 1, TargetNodeID: "new-node"}, automatic)
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("nonvoter assigned with automatic=%v: %v", automatic, err)
		}
	}
	if leader.Status().Coordinator.NodeID != "node-1" {
		t.Fatal("automatic policy moved to nonvoter")
	}
}

func TestIncompleteNonvoterJoinCannotBeTransferred(t *testing.T) {
	c := newTestCluster(t, 1)
	leader := c.leader()
	peer := addUnjoinedTestReplica(t, c, "pending-domain")
	member := Member{NodeID: "new-node", Address: peer.Status().Address, FailureDomain: peer.Status().FailureDomain, StorageLevel: "restricted"}
	// Stop exactly after adding a replica but before join finalization.
	leader.membershipMu.Lock()
	_, err := leader.submit(t.Context(), command{Kind: "join_prepare", ID: "pending-join", Actor: "owner", Member: member})
	if err == nil {
		err = leader.wait(t.Context(), leader.raft.AddNonvoter(raft.ServerID(member.NodeID), raft.ServerAddress(member.Address), leader.Status().ConfigurationIndex, leader.config.ApplyTimeout))
	}
	leader.membershipMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leader.Transfer(t.Context(), TransferRequest{ID: "too-early", Actor: "owner", ExpectedEpoch: 1, TargetNodeID: member.NodeID}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("incomplete nonvoter join transferred: %v", err)
	}
}

func TestReviewedVoteCanReplaceFailedIntentButNotReuseOldRevision(t *testing.T) {
	c := newTestCluster(t, 1)
	leader, _ := joinNonvoter(t, c)
	leader.config.ValidateJoin = func(context.Context, Member) error { return errors.New("test network validation failed") }
	original := VotingRequest{ID: "failed-grant", Actor: "owner", ExpectedRevision: leader.Status().Revision, NodeID: "new-node", Voting: true}
	if _, err := leader.SetVoting(t.Context(), original); !errors.Is(err, ErrNotReady) {
		t.Fatal(err)
	}
	stale := original
	stale.ID = "stale-review"
	stale.Voting = false
	if _, err := leader.SetVoting(t.Context(), stale); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale review replaced pending vote: %v", err)
	}
	revised := stale
	revised.ID = "reviewed-revoke"
	revised.ExpectedRevision = leader.Status().Revision
	before := leader.Status().ConfigurationIndex
	if _, err := leader.SetVoting(t.Context(), revised); err != nil {
		t.Fatal(err)
	}
	leader.config.ValidateJoin = nil
	if _, err := leader.SetVoting(t.Context(), original); !errors.Is(err, ErrConflict) {
		t.Fatalf("superseded grant committed: %v", err)
	}
	if got := leader.Status(); got.Members["new-node"].Voting || got.ConfigurationIndex != before {
		t.Fatal("superseded intent changed consensus")
	}
}

func TestPendingMembershipSurvivesSnapshotAndCannotLeakThroughStateCopy(t *testing.T) {
	m := newMachine("pending", nil)
	member := Member{NodeID: "replica", Address: "addr", StorageLevel: "restricted"}
	m.state.Members[member.NodeID] = member
	m.state.Replicas[member.NodeID] = member.Address
	m.state.PendingJoins = map[string]bool{member.NodeID: true}
	m.state.PendingVotes = map[string]VotingRequest{"other": {ID: "pending-vote", NodeID: "other", Voting: true}}
	copy := m.read()
	delete(copy.PendingJoins, member.NodeID)
	delete(copy.PendingVotes, "other")
	snapshot, err := m.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Release()
	sink := &snapshotMemorySink{}
	if err := snapshot.Persist(sink); err != nil {
		t.Fatal(err)
	}
	restored := newMachine("pending", nil)
	if err := restored.Restore(io.NopCloser(bytes.NewReader(sink.Bytes()))); err != nil {
		t.Fatal(err)
	}
	if restored.state.IsActiveReplica(member.NodeID) || restored.state.PendingVotes["other"].ID != "pending-vote" {
		t.Fatal("snapshot or state copy lost pending membership fences")
	}
}

func TestNonvoterAddressUpdateDoesNotGrantVote(t *testing.T) {
	c := newTestCluster(t, 1)
	leader, peer := joinNonvoter(t, c)
	state := leader.Status()
	if err := leader.waitForProgress(t.Context(), state.Members["new-node"], state.State); err != nil {
		t.Fatal(err)
	}
	// Retaining the Raft endpoint and changing its HTTPS advertisement still
	// exercises the real configuration mutation; it must preserve suffrage.
	if _, err := leader.UpdateMemberAddress(t.Context(), MemberAddressRequest{ID: "nonvoter-address", Actor: "owner", ExpectedRevision: state.Revision, NodeID: "new-node", Address: peer.Status().Address, APIAddress: "https://127.0.0.1:12345"}); err != nil {
		t.Fatal(err)
	}
	if got := leader.Status(); got.Voters["new-node"] != "" || got.Members["new-node"].Voting {
		t.Fatal("address update implicitly granted a vote")
	}
}

func TestVoteProgressTimeoutCanResumeOrBeReReviewed(t *testing.T) {
	for _, sameID := range []bool{true, false} {
		t.Run(map[bool]string{true: "same-id", false: "new-review"}[sameID], func(t *testing.T) {
			c := newTestCluster(t, 1)
			leader, _ := joinNonvoter(t, c)
			leader.config.Probe = func(context.Context, Member) (Progress, error) { return Progress{}, ErrUnavailable }
			request := VotingRequest{ID: "timeout", Actor: "owner", ExpectedRevision: leader.Status().Revision, NodeID: "new-node", Voting: true}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			_, err := leader.SetVoting(ctx, request)
			cancel()
			if !errors.Is(err, ErrNotReady) {
				t.Fatalf("progress timeout: %v", err)
			}
			if leader.Status().PendingVotes[request.NodeID] != request {
				t.Fatal("timed out verification lost resumable intent")
			}
			leader.config.Probe = c.probe
			if !sameID {
				request.ID = "reviewed-after-timeout"
				request.ExpectedRevision = leader.Status().Revision
			}
			if _, err := leader.SetVoting(t.Context(), request); err != nil {
				t.Fatalf("could not resume after timeout: %v", err)
			}
			if got := leader.Status(); !got.Members[request.NodeID].Voting || len(got.PendingVotes) != 0 {
				t.Fatal("resumed operation did not settle")
			}
		})
	}
}
