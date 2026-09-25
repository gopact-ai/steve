package coordination

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/raft"
)

// raftLoopPause parks a node's Raft main loop inside an observer filter.
// requestVote reports the request to observers before it does anything else,
// so a marked RequestVote holds the loop until release. The transport answers
// heartbeats outside the main loop, so a leader keeps contact with the paused
// node, and keeps its lease, while the node acknowledges no appended entry.
type raftLoopPause struct {
	node      *Service
	marker    []byte
	transport *raft.NetworkTransport
	observer  *raft.Observer
	entered   chan struct{}
	released  chan struct{}
	enter     sync.Once
	leave     sync.Once
}

func newRaftLoopPause(t *testing.T, node *Service) *raftLoopPause {
	t.Helper()
	transport, err := raft.NewTCPTransport("127.0.0.1:0", nil, 1, time.Second, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	p := &raftLoopPause{node: node, marker: []byte("pause-" + node.config.NodeID), transport: transport, entered: make(chan struct{}), released: make(chan struct{})}
	p.observer = raft.NewObserver(nil, false, func(o *raft.Observation) bool {
		if request, ok := o.Data.(raft.RequestVoteRequest); ok && bytes.Equal(request.ID, p.marker) {
			p.enter.Do(func() { close(p.entered) })
			<-p.released
		}
		return false
	})
	node.raft.RegisterObserver(p.observer)
	// Registered after the cluster, so it runs before the nodes are closed.
	t.Cleanup(p.release)
	return p
}

// pause returns once the node's main loop is parked. A zero term makes the
// request lose on release, so it leaves no trace in the node's state.
func (p *raftLoopPause) pause() error {
	go func() {
		request := raft.RequestVoteRequest{RPCHeader: raft.RPCHeader{ProtocolVersion: raft.ProtocolVersionMax, ID: p.marker, Addr: []byte(p.transport.LocalAddr())}}
		var response raft.RequestVoteResponse
		p.transport.RequestVote(raft.ServerID(p.node.config.NodeID), p.node.transport.LocalAddr(), &request, &response)
	}()
	select {
	case <-p.entered:
		return nil
	case <-time.After(5 * time.Second):
		return fmt.Errorf("raft loop of %s did not pause", p.node.config.NodeID)
	}
}

func (p *raftLoopPause) release() {
	p.leave.Do(func() {
		close(p.released)
		p.node.raft.DeregisterObserver(p.observer)
		p.transport.Close()
	})
}

func pauseAll(pauses ...*raftLoopPause) error {
	for _, p := range pauses {
		if err := p.pause(); err != nil {
			return err
		}
	}
	return nil
}

// otherVoter returns the voter of a two-voter cluster that is not the leader.
func otherVoter(t *testing.T, c *testCluster, leader *Service) *Service {
	t.Helper()
	for id := range leader.Status().Voters {
		if id != leader.config.NodeID {
			return c.nodes[id]
		}
	}
	t.Fatal("cluster has no second voter")
	return nil
}

// Every membership change waits for its configuration entry while holding
// membershipMu (SetVoting also opMu). Each case pauses followers after the
// call's last quorum write, so that neither the configuration before the
// change nor the one after it has a quorum that acknowledges the entry, while
// the leader keeps its lease. The call must end within ApplyTimeout instead of
// holding the locks until the entry commits.
func TestConfigurationChangeWaitIsBounded(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T) (*Service, func(context.Context) error)
	}{
		{"vote-grant", func(t *testing.T) (*Service, func(context.Context) error) {
			c := newTestCluster(t, 2)
			leader, peer := joinNonvoter(t, c)
			pauses := []*raftLoopPause{newRaftLoopPause(t, otherVoter(t, c, leader)), newRaftLoopPause(t, peer)}
			// The progress probe after the final barrier is the last step
			// before AddVoter; the one before network validation must pass.
			var validated atomic.Bool
			leader.config.ValidateJoin = func(context.Context, Member) error { validated.Store(true); return nil }
			probe := leader.config.Probe
			var once sync.Once
			leader.config.Probe = func(ctx context.Context, member Member) (Progress, error) {
				var err error
				if validated.Load() {
					once.Do(func() { err = pauseAll(pauses...) })
				}
				if err != nil {
					return Progress{}, err
				}
				return probe(ctx, member)
			}
			request := VotingRequest{ID: "grant", Actor: "owner", ExpectedRevision: leader.Status().Revision, NodeID: "new-node", Voting: true}
			return leader, func(ctx context.Context) error { _, err := leader.SetVoting(ctx, request); return err }
		}},
		{"join-voter", func(t *testing.T) (*Service, func(context.Context) error) {
			c := newTestCluster(t, 2)
			leader := c.leader()
			peer := addUnjoinedTestReplica(t, c, "joining-domain")
			pauses := []*raftLoopPause{newRaftLoopPause(t, otherVoter(t, c, leader)), newRaftLoopPause(t, peer)}
			leader.config.ValidateJoin = func(context.Context, Member) error { return pauseAll(pauses...) }
			request := JoinRequest{ID: "join-voter", Actor: "owner", Member: Member{NodeID: "new-node", Address: peer.Status().Address, Voting: true}}
			return leader, func(ctx context.Context) error { _, err := leader.Join(ctx, request); return err }
		}},
		{"address", func(t *testing.T) (*Service, func(context.Context) error) {
			c := newTestCluster(t, 1)
			leader := c.leader()
			peer := addUnjoinedTestReplica(t, c, "voter-domain")
			if _, err := leader.Join(t.Context(), JoinRequest{ID: "join-voter", Actor: "owner", Member: Member{NodeID: "new-node", Address: peer.Status().Address, Voting: true}}); err != nil {
				t.Fatal(err)
			}
			state := leader.Status()
			if err := leader.waitForProgress(t.Context(), state.Members["new-node"], state.State); err != nil {
				t.Fatal(err)
			}
			p := newRaftLoopPause(t, peer)
			leader.config.ValidateAddress = func(context.Context, Member) error { return p.pause() }
			request := MemberAddressRequest{ID: "voter-address", Actor: "owner", ExpectedRevision: state.Revision, NodeID: "new-node", Address: peer.Status().Address, APIAddress: "https://127.0.0.1:12345"}
			return leader, func(ctx context.Context) error { _, err := leader.UpdateMemberAddress(ctx, request); return err }
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			leader, change := tc.run(t)
			finished := make(chan error, 1)
			go func() { finished <- change(t.Context()) }()
			var err error
			select {
			case err = <-finished:
			case <-time.After(10 * time.Second):
				t.Fatal("membership change still waiting for its configuration entry after 10s")
			}
			if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "configuration change") {
				t.Fatalf("uncommitted configuration change returned %v", err)
			}
			if !leader.Status().IsLeader {
				t.Fatal("leader stepped down; the wait was not ended by its bound")
			}
		})
	}
}
