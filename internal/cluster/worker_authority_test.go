package cluster

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/nodewire"
)

// openWorkerTunnel opens coordinator's tunnel to worker's machine and
// completes the handshake a registry makes over it. The channel closes once
// either side drops the tunnel.
func openWorkerTunnel(t *testing.T, coordinator, worker *Peer) <-chan struct{} {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	connection, err := coordinator.DialWorker(ctx, worker.Config.NodeID)
	if err != nil {
		t.Fatalf("open a worker tunnel to %s: %v", worker.Config.NodeID, err)
	}
	if err := connection.SetDeadline(time.Now().Add(nodewire.HandshakeTimeout)); err != nil {
		connection.Close()
		t.Fatal(err)
	}
	if _, err := nodewire.Dial(connection, nodewire.Hello{Token: worker.Worker().Token, Hub: coordinator.Config.ClusterID}); err != nil {
		connection.Close()
		t.Fatalf("handshake over the worker tunnel to %s: %v", worker.Config.NodeID, err)
	}
	if err := connection.SetDeadline(time.Time{}); err != nil {
		connection.Close()
		t.Fatal(err)
	}
	mux := nodewire.NewMux(connection, true)
	t.Cleanup(func() { mux.Close() })
	return mux.Done()
}

// joinNonvoter starts a peer that joins hub as a non-voting member. A
// non-nil raft receives the member's inbound Raft connections.
func joinNonvoter(t *testing.T, hub *Peer, raft *gatedListener) *Peer {
	t.Helper()
	options, _ := testPeerOptions(t, ClusterPeerTestDir(t), hub)
	var activations atomic.Int32
	options.Activate = testPeerApplication(t, &activations)
	if raft != nil {
		var bound atomic.Bool
		options.Listen = func(network, address string) (net.Listener, error) {
			listener, err := net.Listen(network, address)
			// The Raft listener is the first the peer binds.
			if err != nil || !bound.CompareAndSwap(false, true) {
				return listener, err
			}
			raft.Listener = listener
			return raft, nil
		}
	}
	member := StartTestPeer(t, options)
	if raft != nil {
		t.Cleanup(raft.resume)
	}
	if _, err := hub.Join(t.Context(), coordination.JoinRequest{ID: "join-" + member.Config.NodeID, Actor: "owner", Member: coordination.Member{NodeID: member.Config.NodeID, Name: member.Config.Name, Address: member.Config.RaftAddress, APIAddress: member.Config.PeerURL, Voting: false}}); err != nil {
		t.Fatal(err)
	}
	return member
}

func startTestHub(t *testing.T) *Peer {
	t.Helper()
	options, _ := testPeerOptions(t, ClusterPeerTestDir(t), nil)
	var activations atomic.Int32
	options.Activate = testPeerApplication(t, &activations)
	hub := StartTestPeer(t, options)
	WaitPeerReady(t, hub)
	return hub
}

// An idle worker tunnel costs the cluster nothing. Every quorum read the
// consensus leader serves appends a barrier to its log, whether it is its
// own or a member's request for the leader's state, so a log that does not
// grow also shows that no member asked the leader for its state.
func TestIdleWorkerTunnelsAppendNothingToTheConsensusLog(t *testing.T) {
	hub := startTestHub(t)
	member := joinNonvoter(t, hub, nil)
	WaitPeerReady(t, hub)
	service := hub.Runtime.Load().service
	growth := func() uint64 {
		time.Sleep(time.Second)
		before := service.LastIndex()
		time.Sleep(3 * time.Second)
		return service.LastIndex() - before
	}
	if grew := growth(); grew != 0 {
		t.Fatalf("with no worker tunnel open the leader's log grew by %d entries in 3s; the measurement needs an idle cluster", grew)
	}
	openWorkerTunnel(t, hub, hub)
	if grew := growth(); grew != 0 {
		t.Fatalf("an idle tunnel to the coordinator's own worker grew the leader's log by %d entries in 3s; its authority is being confirmed by quorum reads", grew)
	}
	openWorkerTunnel(t, hub, member)
	if grew := growth(); grew != 0 {
		t.Fatalf("an idle tunnel to a non-voting member's worker grew the leader's log by %d entries in 3s; the member is asking the leader for its state", grew)
	}
}

// A revoked business generation drops its worker tunnels at once, not once
// its stop has finished and a later generation has taken over.
func TestRevokedGenerationClosesItsWorkerTunnelsWhileItStops(t *testing.T) {
	options, _ := testPeerOptions(t, ClusterPeerTestDir(t), nil)
	var activations atomic.Int32
	start := testPeerApplication(t, &activations)
	release := make(chan struct{})
	options.Activate = func(ctx context.Context, activation Activation, ready func(PeerApplicationEndpoint) error) (Deactivate, error) {
		stop, err := start(ctx, activation, ready)
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context) error { <-release; return stop(ctx) }, nil
	}
	hub := StartTestPeer(t, options)
	var released atomic.Bool
	unblock := func() {
		if released.CompareAndSwap(false, true) {
			close(release)
		}
	}
	t.Cleanup(unblock)
	WaitPeerReady(t, hub)
	closed := openWorkerTunnel(t, hub, hub)
	runtime := hub.Runtime.Load()
	runtime.mu.Lock()
	current := runtime.current
	runtime.mu.Unlock()
	// As a write whose outcome could not be confirmed does.
	runtime.revoke(current, fmt.Errorf("%w: revoked by the test", coordination.ErrUnavailable))
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("the worker tunnel of a revoked business generation stayed open while the generation was stopping")
	}
	unblock()
}

// A worker whose replica stops hearing the consensus leader cannot tell
// whether the coordinator it serves was replaced, so it drops the tunnel,
// and refuses a new one until its replica hears the leader again.
func TestWorkerTunnelClosesWhenItsReplicaStopsHearingTheLeader(t *testing.T) {
	hub := startTestHub(t)
	raft := &gatedListener{}
	member := joinNonvoter(t, hub, raft)
	WaitPeerReady(t, hub)
	closed := openWorkerTunnel(t, hub, member)
	raft.pause()
	// Raft's follower notices the silence within twice its heartbeat
	// timeout, one second by default, and gives up after ApplyTimeout.
	bound := 2*time.Second + member.Runtime.Load().config.Coordination.ApplyTimeout + 3*time.Second
	select {
	case <-closed:
	case <-time.After(bound):
		t.Fatalf("the worker kept its tunnel %s after its replica stopped hearing the consensus leader", bound)
	}
	if !hub.Runtime.Load().Status().Ready {
		t.Fatal("the coordinator lost its business generation; the tunnel was not closed by the worker")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if connection, err := hub.DialWorker(ctx, member.Config.NodeID); err == nil {
		connection.Close()
		t.Fatal("a worker whose replica hears no consensus leader accepted a new tunnel")
	}
}

// Removing a machine from the cluster drops the coordinator's tunnel to
// its worker.
func TestRemovingAMachineClosesTheTunnelToItsWorker(t *testing.T) {
	hub := startTestHub(t)
	member := joinNonvoter(t, hub, nil)
	WaitPeerReady(t, hub)
	closed := openWorkerTunnel(t, hub, member)
	if _, err := hub.Runtime.Load().Remove(t.Context(), coordination.RemoveRequest{ID: "remove-member", Actor: "owner", NodeID: member.Config.NodeID}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("the tunnel to a removed machine's worker stayed open")
	}
}
