package cluster

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/nodewire"
)

// openWorkerTunnel opens coordinator's tunnel to worker's machine and
// completes the handshake a registry makes over it. The mux is done once
// either side drops the tunnel.
func openWorkerTunnel(t *testing.T, coordinator, worker *Peer) *nodewire.Mux {
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
	return mux
}

// joinNonvoter starts a peer that joins hub as a non-voting member. A
// non-nil raft wraps the listener of the member's inbound Raft connections.
func joinNonvoter(t *testing.T, hub *Peer, raft func(net.Listener) net.Listener) *Peer {
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
			return raft(listener), nil
		}
	}
	member := StartTestPeer(t, options)
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

// entryGate wraps a Raft listener. While paused it holds each inbound
// connection from its first read larger than a heartbeat: log entries stop
// arriving, and heartbeats, which travel on connections of their own, keep
// arriving.
type entryGate struct {
	net.Listener
	paused atomic.Bool
	hold   chan struct{}
	once   sync.Once
}

func newEntryGate() *entryGate { return &entryGate{hold: make(chan struct{})} }

func (g *entryGate) wrap(listener net.Listener) net.Listener {
	g.Listener = listener
	return g
}

func (g *entryGate) release() { g.once.Do(func() { close(g.hold) }) }

func (g *entryGate) Accept() (net.Conn, error) {
	conn, err := g.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &entryGateConn{Conn: conn, gate: g}, nil
}

type entryGateConn struct {
	net.Conn
	gate *entryGate
	held bool
}

func (c *entryGateConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if c.gate.paused.Load() && (c.held || n > 1024) {
		c.held = true
		<-c.gate.hold
	}
	return n, err
}

// transferCoordinator names to as the coordinator. Its large reason makes
// the entry larger than any heartbeat.
func transferCoordinator(t *testing.T, through, to *Peer, reason string) {
	t.Helper()
	runtime := through.Runtime.Load()
	state, err := runtime.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Transfer(t.Context(), coordination.TransferRequest{ID: "transfer-to-" + to.Config.NodeID + "-" + strconv.FormatUint(state.Coordinator.Epoch, 10), Actor: "owner", ExpectedEpoch: state.Coordinator.Epoch, TargetNodeID: to.Config.NodeID, Reason: reason}); err != nil {
		t.Fatalf("name %s the coordinator: %v", to.Config.NodeID, err)
	}
}

// A coordinator that was replaced while its replica received heartbeats
// but no log entries still names itself, so no local view tells it or its
// own worker that it no longer coordinates. The tunnel to its own machine's
// worker, on which commands, file writes and agents need no further check,
// is closed nonetheless within two ApplyTimeouts.
func TestReplacedCoordinatorLosesItsOwnWorkerTunnelWhileItsReplicationLags(t *testing.T) {
	hub := startTestHub(t)
	gate := newEntryGate()
	old := joinNonvoter(t, hub, gate.wrap)
	t.Cleanup(gate.release)
	transferCoordinator(t, hub, old, "hand over")
	WaitPeerReady(t, old)
	closed := openWorkerTunnel(t, old, old).Done()
	gate.paused.Store(true)
	transferCoordinator(t, hub, hub, strings.Repeat("take back ", 1600))
	replaced := time.Now()
	WaitPeerReady(t, hub)
	if seen := old.Runtime.Load().Status().Coordinator; seen.NodeID != old.Config.NodeID {
		t.Fatalf("the replaced coordinator's replica applied the transfer to %s; its replication was meant to lag", seen.NodeID)
	}
	timeout := old.Runtime.Load().config.Coordination.ApplyTimeout
	bound := 2*timeout + 3*time.Second
	select {
	case <-closed:
		if seen := old.Runtime.Load().Status().Coordinator; seen.NodeID != old.Config.NodeID {
			t.Fatalf("the tunnel closed once the replica caught up with the transfer, not while it lagged")
		}
		t.Logf("the tunnel closed %s after the coordinator was replaced", time.Since(replaced).Round(time.Millisecond))
	case <-time.After(bound - time.Since(replaced)):
		t.Fatalf("the replaced coordinator kept the tunnel to its own worker %s after it was replaced, while its replication lagged", bound)
	}
}

// An idle worker tunnel appends nothing to the consensus log. Every quorum
// read the consensus leader serves appends a barrier to it, whether it is
// its own or a member's request for the leader's state, so a log that does
// not grow also shows that no member asked the leader for its state. The
// member confirms its replica with a read index instead, which the
// measurement spans.
func TestIdleWorkerTunnelsAppendNothingToTheConsensusLog(t *testing.T) {
	hub := startTestHub(t)
	member := joinNonvoter(t, hub, nil)
	WaitPeerReady(t, hub)
	service := hub.Runtime.Load().service
	span := hub.Runtime.Load().config.Coordination.ApplyTimeout + time.Second
	growth := func() uint64 {
		time.Sleep(time.Second)
		before := service.LastIndex()
		time.Sleep(span)
		return service.LastIndex() - before
	}
	confirmed := func() time.Time {
		workers := &member.Runtime.Load().workers
		workers.mu.Lock()
		defer workers.mu.Unlock()
		return workers.confirmed
	}
	if grew := growth(); grew != 0 {
		t.Fatalf("with no worker tunnel open the leader's log grew by %d entries in %s; the measurement needs an idle cluster", grew, span)
	}
	own := openWorkerTunnel(t, hub, hub)
	if grew := growth(); grew != 0 {
		t.Errorf("an idle tunnel to the coordinator's own worker grew the leader's log by %d entries in %s; its authority is being confirmed by quorum reads", grew, span)
	}
	own.Close()
	openWorkerTunnel(t, hub, member)
	opened := confirmed()
	if grew := growth(); grew != 0 {
		t.Errorf("an idle tunnel to a non-voting member's worker grew the leader's log by %d entries in %s; the member is asking the leader for its state", grew, span)
	}
	if !confirmed().After(opened) {
		t.Errorf("the member kept its worker tunnel %s without confirming its replica with a quorum", span+time.Second)
	}
}

// A revoked business generation drops its worker tunnels at once, not once
// its stop has finished and a later generation has taken over: the tunnel
// to its own machine's worker, and those to other machines, whose replicas
// would only notice once a later generation is named.
func TestRevokedGenerationClosesItsWorkerTunnelsWhileItStops(t *testing.T) {
	t.Run("own worker", func(t *testing.T) { testRevokedGenerationClosesItsWorkerTunnel(t, false) })
	t.Run("another machine's worker", func(t *testing.T) { testRevokedGenerationClosesItsWorkerTunnel(t, true) })
}

func testRevokedGenerationClosesItsWorkerTunnel(t *testing.T, remote bool) {
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
	worker := hub
	if remote {
		worker = joinNonvoter(t, hub, nil)
		WaitPeerReady(t, hub)
	}
	closed := openWorkerTunnel(t, hub, worker).Done()
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
	member := joinNonvoter(t, hub, raft.wrap)
	t.Cleanup(raft.resume)
	WaitPeerReady(t, hub)
	closed := openWorkerTunnel(t, hub, member).Done()
	raft.pause()
	// Raft's follower forgets its leader within a few of its one-second
	// heartbeat timeouts, and the worker gives up ApplyTimeout later.
	bound := 3*time.Second + member.Runtime.Load().config.Coordination.ApplyTimeout + 4*time.Second
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
	closed := openWorkerTunnel(t, hub, member).Done()
	if _, err := hub.Runtime.Load().Remove(t.Context(), coordination.RemoveRequest{ID: "remove-member", Actor: "owner", NodeID: member.Config.NodeID}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("the tunnel to a removed machine's worker stayed open")
	}
}

// A worker tunnel's grant lapses on the same judgment of the local replica
// that gives up a business generation, and on any change to the authority
// it was opened under. Observations carry their own times, so nothing here
// depends on scheduling.
func TestWorkerGrantLapsesWithTheLocalReplica(t *testing.T) {
	const timeout = time.Second
	config := Config{Coordination: coordination.Config{NodeID: "node-1", ApplyTimeout: timeout}}
	assignment := coordination.Assignment{NodeID: "node-3", Epoch: 4}
	started := time.Now()
	at := func(after time.Duration, change func(*observation)) observation {
		seen := observation{at: started.Add(after), log: coordination.LogProgress{Committed: 10, Applied: 10}}
		seen.Healthy, seen.LeaderID = true, "node-2"
		seen.Coordinator, seen.WriterGeneration = assignment, 7
		seen.Members = map[string]coordination.Member{"node-1": {NodeID: "node-1"}, "node-3": {NodeID: "node-3"}}
		if change != nil {
			change(&seen)
		}
		return seen
	}
	noLeader := func(seen *observation) { seen.LeaderID = "" }
	for _, tc := range []struct {
		name string
		seen []observation
		want error
	}{
		{name: "kept", seen: []observation{at(0, nil), at(timeout, nil), at(3*timeout, nil)}},
		{name: "replica unhealthy", seen: []observation{at(0, nil), at(time.Millisecond, func(seen *observation) { seen.Healthy = false })}, want: coordination.ErrUnavailable},
		{name: "another coordinator", seen: []observation{at(0, nil), at(time.Millisecond, func(seen *observation) { seen.Coordinator.Epoch++ })}, want: coordination.ErrStaleEpoch},
		{name: "another writer generation", seen: []observation{at(0, nil), at(time.Millisecond, func(seen *observation) { seen.WriterGeneration++ })}, want: coordination.ErrStaleWriter},
		{name: "worker's machine removed", seen: []observation{at(0, nil), at(time.Millisecond, func(seen *observation) { delete(seen.Members, "node-1") })}, want: coordination.ErrInvalid},
		{name: "follower's leader late", seen: []observation{at(0, nil), at(timeout, noLeader)}},
		{name: "follower hears no leader", seen: []observation{at(0, nil), at(timeout, noLeader), at(timeout+time.Millisecond, noLeader)}, want: coordination.ErrUnavailable},
		{name: "leader steps down knowing no other", seen: []observation{at(0, func(seen *observation) { seen.LeaderID = "node-1" }), at(time.Millisecond, noLeader)}, want: coordination.ErrUnavailable},
		{name: "committed entry stays unapplied", seen: []observation{at(0, func(seen *observation) { seen.log.Committed = 12 }), at(timeout+time.Millisecond, func(seen *observation) { seen.log.Committed = 12 })}, want: coordination.ErrUnavailable},
		{name: "steady writes", seen: []observation{
			at(0, func(seen *observation) { seen.log = coordination.LogProgress{Committed: 12, Applied: 10} }),
			at(timeout, func(seen *observation) { seen.log = coordination.LogProgress{Committed: 14, Applied: 12} }),
			at(2*timeout, func(seen *observation) { seen.log = coordination.LogProgress{Committed: 16, Applied: 14} }),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime := &Runtime{config: config, workers: workerAuthority{live: liveness{started: started, heard: started}}}
			var err error
			for _, seen := range tc.seen {
				// Each observation here follows a quorum confirmation; see
				// TestWorkerTunnelsOfAFollowerNeedQuorumConfirmations.
				runtime.workers.confirmed = seen.at
				if _, err = runtime.authorizesWorker(seen, "node-1", assignment, 7); err != nil {
					break
				}
			}
			if tc.want == nil && err != nil {
				t.Fatalf("grant lapsed: %v", err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("grant lapsed with %v, want %v", err, tc.want)
			}
		})
	}
}

// The worker tunnels of a node share one judgment of its replica, which
// also admits new ones: once a tunnel lapses because the replica stopped
// hearing the consensus leader, a tunnel asked for a moment later is
// refused on the same grounds, not admitted on a view that has yet to
// notice.
func TestWorkerTunnelIsAdmittedOnTheJudgmentThatClosedAnother(t *testing.T) {
	const timeout = time.Second
	runtime := &Runtime{config: Config{Coordination: coordination.Config{NodeID: "node-1", ApplyTimeout: timeout}}}
	started := time.Now()
	runtime.workers = workerAuthority{live: liveness{started: started}}
	assignment := coordination.Assignment{NodeID: "node-3", Epoch: 4}
	at := func(after time.Duration, leader string) observation {
		seen := observation{at: started.Add(after), log: coordination.LogProgress{Committed: 10, Applied: 10}}
		seen.Healthy, seen.LeaderID = true, leader
		seen.Coordinator, seen.WriterGeneration = assignment, 7
		seen.Members = map[string]coordination.Member{"node-1": {NodeID: "node-1"}, "node-3": {NodeID: "node-3"}}
		runtime.workers.confirmed = seen.at
		return seen
	}
	// The open tunnel last saw the leader at 0; another observer, the one
	// admitting the next tunnel, saw it 50ms later.
	if _, err := runtime.authorizesWorker(at(0, "node-2"), "node-1", assignment, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.authorizesWorker(at(50*time.Millisecond, "node-2"), "node-1", assignment, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.authorizesWorker(at(timeout+60*time.Millisecond, ""), "node-1", assignment, 7); !errors.Is(err, coordination.ErrUnavailable) {
		t.Fatalf("the open tunnel was kept %s after the replica last heard a leader: %v", timeout+10*time.Millisecond, err)
	}
	if _, err := runtime.authorizesWorker(at(timeout+70*time.Millisecond, ""), "node-1", assignment, 7); !errors.Is(err, coordination.ErrUnavailable) {
		t.Fatalf("a tunnel was admitted just after another closed because the replica stopped hearing the leader: %v", err)
	}
}

// A node that does not lead consensus keeps its worker tunnels only while a
// quorum keeps confirming its replica: every ApplyTimeout it starts a
// confirmation, one at a time, and its tunnels lapse when one fails or
// none has succeeded for three ApplyTimeouts. A leader needs none.
func TestWorkerTunnelsOfAFollowerNeedQuorumConfirmations(t *testing.T) {
	const timeout = time.Second
	started := time.Now()
	failed := fmt.Errorf("%w: the read index could not be reached", coordination.ErrUnavailable)
	type step struct {
		// Either an observation at after, by a node that hears leader...
		after  time.Duration
		leader string
		// ...expecting a confirmation to start and err, or the result of a
		// confirmation that started at after.
		confirm bool
		err     error
		settle  bool
		result  error
	}
	observe := func(after time.Duration, leader string, confirm bool, err error) step {
		return step{after: after, leader: leader, confirm: confirm, err: err}
	}
	settle := func(after time.Duration, result error) step {
		return step{after: after, settle: true, result: result}
	}
	const follower, leader = "node-2", "node-1"
	for _, tc := range []struct {
		name  string
		steps []step
	}{
		{name: "leader needs none", steps: []step{
			observe(0, leader, false, nil),
			observe(10*timeout, leader, false, nil),
		}},
		{name: "follower confirms every ApplyTimeout, one at a time", steps: []step{
			observe(timeout-time.Millisecond, follower, false, nil),
			observe(timeout, follower, true, nil),
			observe(timeout+time.Millisecond, follower, false, nil),
			settle(timeout, nil),
			observe(2*timeout-time.Millisecond, follower, false, nil),
			observe(2*timeout, follower, true, nil),
		}},
		{name: "failed confirmation", steps: []step{
			observe(timeout, follower, true, nil),
			settle(timeout, failed),
			observe(timeout+time.Millisecond, follower, false, coordination.ErrUnavailable),
		}},
		{name: "a failure older than the latest confirmation", steps: []step{
			observe(timeout, follower, true, nil),
			settle(timeout+time.Millisecond, nil),
			settle(timeout, failed),
			observe(timeout+2*time.Millisecond, follower, false, nil),
		}},
		{name: "confirmation that never finishes", steps: []step{
			observe(timeout, follower, true, nil),
			observe(3*timeout, follower, false, nil),
			observe(3*timeout+time.Millisecond, follower, false, coordination.ErrUnavailable),
		}},
		{name: "leading counts as confirmed", steps: []step{
			observe(5*timeout/2, leader, false, nil),
			observe(3*timeout, follower, false, nil),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime := &Runtime{config: Config{Coordination: coordination.Config{NodeID: leader, ApplyTimeout: timeout}}}
			// Opening a tunnel confirmed the replica at 0.
			runtime.workers = workerAuthority{live: liveness{started: started, heard: started}, confirmed: started}
			for i, s := range tc.steps {
				if s.settle {
					runtime.workers.mu.Lock()
					runtime.workers.confirming = false
					runtime.workers.mu.Unlock()
					runtime.workers.settle(started.Add(s.after), s.result)
					continue
				}
				seen := observation{at: started.Add(s.after), log: coordination.LogProgress{Committed: 10, Applied: 10}}
				seen.Healthy, seen.LeaderID = true, s.leader
				confirm, err := runtime.keepsWorkers(seen)
				if confirm != s.confirm {
					t.Fatalf("step %d, at %s: started a confirmation: %v, want %v", i, s.after, confirm, s.confirm)
				}
				if s.err == nil && err != nil {
					t.Fatalf("step %d, at %s: tunnels lapsed: %v", i, s.after, err)
				}
				if s.err != nil && !errors.Is(err, s.err) {
					t.Fatalf("step %d, at %s: tunnels lapsed with %v, want %v", i, s.after, err, s.err)
				}
			}
		})
	}
}

// A worker tunnel belongs to a business generation that is running: not
// to one that has ended, nor to one whose replica is being restored from a
// snapshot, which ends it.
func TestWorkerTunnelsBelongToARunningGeneration(t *testing.T) {
	assignment := coordination.Assignment{NodeID: "node-1", Epoch: 3}
	for _, tc := range []struct {
		name    string
		change  func(*Runtime)
		running bool
	}{
		{name: "running", running: true},
		{name: "ended", change: func(r *Runtime) { r.current.cancel() }},
		{name: "restoring", change: func(r *Runtime) { r.restoring = true }},
		{name: "restored", change: func(r *Runtime) { r.restores++ }},
		{name: "closed", change: func(r *Runtime) { r.closed = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			runtime := &Runtime{current: &generation{Activation: Activation{Assignment: assignment, WriterGeneration: 7, Context: ctx}, cancel: cancel}}
			if tc.change != nil {
				tc.change(runtime)
			}
			_, _, runningErr := runtime.running()
			_, doneErr := runtime.generationDone(assignment, 7)
			if tc.running && (runningErr != nil || doneErr != nil) {
				t.Fatalf("the running generation dials no worker: %v, %v", runningErr, doneErr)
			}
			if !tc.running && (!errors.Is(runningErr, ErrInactive) || !errors.Is(doneErr, ErrInactive)) {
				t.Fatalf("a generation that is not running could dial a worker: %v, %v", runningErr, doneErr)
			}
		})
	}
}

// A registry dials workers for the business generation that configured
// it: once a later generation runs, it dials nothing more.
func TestWorkerDialerDialsForItsOwnGenerationOnly(t *testing.T) {
	hub := startTestHub(t)
	runtime := hub.Runtime.Load()
	assignment, writer, err := runtime.running()
	if err != nil {
		t.Fatal(err)
	}
	dial := hub.workerDialer(runtime, assignment, writer)
	runtime.mu.Lock()
	current := runtime.current
	runtime.mu.Unlock()
	runtime.revoke(current, fmt.Errorf("%w: revoked by the test", coordination.ErrUnavailable))
	WaitPeerReady(t, hub)
	if _, later, _ := runtime.running(); later == writer {
		t.Fatalf("the generation was not rebuilt; it still runs at writer generation %d", writer)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if connection, err := dial(ctx, hub.Config.NodeID); !errors.Is(err, ErrInactive) {
		if err == nil {
			connection.Close()
		}
		t.Fatalf("the dialer of a replaced generation opened a tunnel for its successor: %v", err)
	}
	// Nodes configured once their generation had ended get a dialer bound
	// to none.
	if connection, err := hub.workerDialer(runtime, coordination.Assignment{}, 0)(ctx, hub.Config.NodeID); !errors.Is(err, ErrInactive) {
		if err == nil {
			connection.Close()
		}
		t.Fatalf("a dialer bound to no generation opened a tunnel: %v", err)
	}
	if connection, err := hub.DialWorker(ctx, hub.Config.NodeID); err != nil {
		t.Fatalf("the running generation could not dial its own worker: %v", err)
	} else {
		connection.Close()
	}
}
