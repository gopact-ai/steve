package coordination

import (
	"context"
	"errors"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/raft"
)

// Replication and the control command still proceed while a heartbeat worker
// waits in its real transport. Service closure must wait for that worker too.
type shutdownReplicationStream struct {
	shutdownTCPStream
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *shutdownReplicationStream) Dial(address raft.ServerAddress, timeout time.Duration) (net.Conn, error) {
	if s.armed.Load() {
		pc := make([]uintptr, 32)
		frames := runtime.CallersFrames(pc[:runtime.Callers(2, pc)])
		for {
			frame, more := frames.Next()
			if frame.Function == "github.com/hashicorp/raft.(*Raft).heartbeat" {
				s.once.Do(func() { close(s.entered); <-s.release })
				break
			}
			if !more {
				break
			}
		}
	}
	return s.shutdownTCPStream.Dial(address, timeout)
}

func TestServiceCloseWaitsForRaftWorkersAfterLosingItsVote(t *testing.T) {
	var stream *shutdownReplicationStream
	c := newTunedTestCluster(t, 3, func(config *Config) {
		if config.NodeID != "node-1" {
			return
		}
		listener, err := net.Listen("tcp", config.BindAddress)
		if err != nil {
			t.Fatal(err)
		}
		stream = &shutdownReplicationStream{shutdownTCPStream: shutdownTCPStream{listener}, entered: make(chan struct{}), release: make(chan struct{})}
		config.StreamLayer = stream
		config.RaftConfig.ShutdownOnRemove = true
	})
	var release sync.Once
	unpause := func() { release.Do(func() { close(stream.release) }) }
	defer unpause()
	leader := c.leader()
	if leader.config.NodeID != "node-1" {
		t.Fatal("test requires the initial consensus leader")
	}
	if _, err := leader.Transfer(t.Context(), TransferRequest{ID: "move-business-before-demotion", Actor: "owner", ExpectedEpoch: leader.Status().Coordinator.Epoch, TargetNodeID: "node-2"}); err != nil {
		t.Fatal(err)
	}
	if !leader.Status().IsLeader {
		t.Fatal("business transfer unexpectedly moved consensus leadership")
	}
	stream.armed.Store(true)
	leader.transport.CloseStreams()
	waitShutdownSignal(t, stream.entered, "outgoing Raft heartbeat")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	changed := make(chan error, 1)
	request := VotingRequest{ID: "demote-consensus-leader", Actor: "owner", ExpectedRevision: leader.Status().Revision, NodeID: leader.config.NodeID, Voting: false}
	go func() {
		_, err := leader.SetVoting(ctx, request)
		changed <- err
	}()
	eventually(t, 4*time.Second, func() bool {
		return leader.raft.State() != raft.Leader && leader.Status().Voters[leader.config.NodeID] == ""
	})
	closed := make(chan error, 2)
	go func() { closed <- leader.Close() }()
	go func() { closed <- leader.Close() }()
	assertShutdownWaits(t, leader, closed, unpause, 2)
	select {
	case err := <-changed:
		if err != nil && !errors.Is(err, ErrNotLeader) && !errors.Is(err, ErrUnavailable) {
			t.Fatalf("demotion result: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("demotion survived the drained service")
	}
	if err := leader.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDemotedConsensusLeaderRemainsAReplicaAndCanRegainItsVote(t *testing.T) {
	c := newTunedTestCluster(t, 3, func(config *Config) { config.RaftConfig.ShutdownOnRemove = true })
	for id, config := range c.configs {
		if !config.RaftConfig.ShutdownOnRemove {
			t.Fatalf("opening %s changed the caller's configuration", id)
		}
	}
	original := c.leader()
	if original.config.NodeID != "node-1" {
		t.Fatal("test requires the initial consensus leader")
	}
	if _, err := original.Transfer(t.Context(), TransferRequest{ID: "move-business", Actor: "owner", ExpectedEpoch: original.Status().Coordinator.Epoch, TargetNodeID: "node-2"}); err != nil {
		t.Fatal(err)
	}
	request := VotingRequest{ID: "revoke-former-coordinator-vote", Actor: "owner", ExpectedRevision: original.Status().Revision, NodeID: original.config.NodeID, Voting: false}
	_, err := original.SetVoting(t.Context(), request)
	if err != nil && !errors.Is(err, ErrNotLeader) && !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	eventually(t, 4*time.Second, func() bool {
		return original.raft.State() != raft.Leader && original.Status().Voters[original.config.NodeID] == ""
	})
	if status := original.Status(); !status.Healthy || status.Coordinator.NodeID == original.config.NodeID || !status.IsActiveReplica(original.config.NodeID) {
		t.Fatalf("demotion closed a replica or retained business authority: %+v", status)
	}
	leader := c.leader()
	if _, err := leader.SetVoting(t.Context(), request); err != nil {
		t.Fatalf("finish demotion on the new leader: %v", err)
	}
	state, err := leader.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if state.Members[original.config.NodeID].Voting || state.Voters[original.config.NodeID] != "" {
		t.Fatalf("vote not revoked: %+v", state)
	}
	if _, err := leader.SetVoting(t.Context(), VotingRequest{ID: "grant-again", Actor: "owner", ExpectedRevision: state.Revision, NodeID: original.config.NodeID, Voting: true}); err != nil {
		t.Fatalf("a healthy nonvoting replica could not regain its vote: %v", err)
	}
	if !original.Status().Healthy {
		t.Fatal("granting a vote required reopening the replica")
	}
	if _, err := leader.Remove(t.Context(), RemoveRequest{ID: "remove-after-grant", Actor: "owner", NodeID: original.config.NodeID}); err != nil {
		t.Fatal(err)
	}
	eventually(t, 4*time.Second, func() bool { return !original.Status().IsActiveReplica(original.config.NodeID) })
	if original.Status().IsLeader || original.Status().Coordinator.NodeID == original.config.NodeID {
		t.Fatal("a removed replica kept consensus or business leadership")
	}
	if err := original.Close(); err != nil {
		t.Fatal(err)
	}
	if err := original.Close(); err != nil {
		t.Fatal(err)
	}
}
