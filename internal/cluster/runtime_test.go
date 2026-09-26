package cluster

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/ledger"
	statepkg "github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/hashicorp/raft"
)

type businessStores struct {
	activation Activation
	state      *statepkg.Store
	tasks      *task.Store
}

type clusterNode struct {
	runtime   atomic.Pointer[Runtime]
	dropApp   atomic.Bool
	holdApp   atomic.Bool
	heldApp   chan coordination.AppCommand
	gateApp   atomic.Pointer[appGate]
	gateFence atomic.Pointer[appGate]
	served    sync.Map // RPC path -> *atomic.Int64 requests this node's server received
	raft      *gatedListener
	server    *httptest.Server
	listener  net.Listener
	options   coordination.TLSOptions
	client    *coordination.Client
	config    Config
	mu        sync.Mutex
	business  *businessStores
	stopped   int
	activated []*businessStores
}

// appGate stops application commands (gateApp) or writer fences (gateFence)
// at the consensus leader's RPC handler until release is closed, then serves
// them unchanged.
type appGate struct {
	once    sync.Once
	arrived chan struct{}
	release chan struct{}
}

// gatedListener accepts the node's inbound Raft connections. While paused,
// bytes read from them are held back from Raft until resume.
type gatedListener struct {
	net.Listener
	gate atomic.Pointer[chan struct{}]
}

func (l *gatedListener) pause() {
	gate := make(chan struct{})
	l.gate.CompareAndSwap(nil, &gate)
}

func (l *gatedListener) resume() {
	if gate := l.gate.Swap(nil); gate != nil {
		close(*gate)
	}
}

func (l *gatedListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return gatedConn{Conn: conn, listener: l}, nil
}

type gatedConn struct {
	net.Conn
	listener *gatedListener
}

func (c gatedConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if gate := c.listener.gate.Load(); gate != nil {
		<-*gate
	}
	return n, err
}

// servedCount is how many requests for path this node's server received.
func (n *clusterNode) servedCount(path string) int64 {
	if counter, ok := n.served.Load(path); ok {
		return counter.(*atomic.Int64).Load()
	}
	return 0
}

func (n *clusterNode) current() *businessStores {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.business
}

// fixtureTiming is the Raft timing of the nodes a fixture builds and how
// long their clients keep trying members while none takes a call.
type fixtureTiming struct {
	heartbeat, election, lease, retryWindow time.Duration
}

// fastTiming notices a lost leader within a few hundred milliseconds, so
// tests that replace a failed leader or coordinator finish quickly.
var fastTiming = fixtureTiming{heartbeat: 180 * time.Millisecond, election: 180 * time.Millisecond, lease: 90 * time.Millisecond}

// steadyTiming is Raft's default timing, for tests that need the consensus
// leader to stay where it is. Under fastTiming a leader that the race
// detector and a single CPU keep busy for longer than its 90ms lease steps
// down, and another member may take over. An election takes longer here, so
// the clients keep trying for longer.
var steadyTiming = fixtureTiming{heartbeat: time.Second, election: time.Second, lease: 500 * time.Millisecond, retryWindow: 15 * time.Second}

func testNodes(t *testing.T, count int) []*clusterNode {
	t.Helper()
	return testNodesWith(t, count, fastTiming)
}

func testNodesWith(t *testing.T, count int, timing fixtureTiming) []*clusterNode {
	t.Helper()
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test cluster"}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, public, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	nodes := make([]*clusterNode, count)
	var members []coordination.Member
	for i := range nodes {
		id := fmt.Sprintf("node-%d", i+1)
		pub, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		leaf := &x509.Certificate{SerialNumber: big.NewInt(int64(i + 2)), Subject: pkix.Name{CommonName: id}, NotBefore: ca.NotBefore, NotAfter: ca.NotAfter,
			KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}, URIs: []*url.URL{coordination.IdentityURI("test-cluster", id)}}
		der, err := x509.CreateCertificate(rand.Reader, leaf, ca, pub, key)
		if err != nil {
			t.Fatal(err)
		}
		n := &clusterNode{heldApp: make(chan coordination.AppCommand, 32), options: coordination.TLSOptions{ClusterID: "test-cluster", NodeID: id, RootCAs: pool, Certificate: tls.Certificate{Certificate: [][]byte{der, caDER}, PrivateKey: private}}}
		raw, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		n.raft = &gatedListener{Listener: raw}
		n.listener = n.raft
		n.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			counter, _ := n.served.LoadOrStore(r.URL.Path, new(atomic.Int64))
			counter.(*atomic.Int64).Add(1)
			if current := n.runtime.Load(); current != nil {
				// A node authorizes a forwarded control command as the owner,
				// the actor the local caller used, so a command forwarded
				// after a partial local attempt keeps its fingerprint.
				handler := current.RPCHandler(coordination.RPCOptions{AuthorizeControl: func(*http.Request, coordination.Identity, string) (string, error) { return "owner", nil }})
				if gate := n.gateApp.Load(); gate != nil && r.URL.Path == coordination.RPCPath+"app" {
					gate.once.Do(func() { close(gate.arrived) })
					<-gate.release
				}
				if gate := n.gateFence.Load(); gate != nil && r.URL.Path == coordination.RPCPath+"writer" {
					gate.once.Do(func() { close(gate.arrived) })
					<-gate.release
				}
				if n.holdApp.Load() && r.URL.Path == coordination.RPCPath+"app" {
					var command coordination.AppCommand
					if err := json.NewDecoder(r.Body).Decode(&command); err != nil {
						http.Error(w, "invalid test command", http.StatusBadRequest)
						return
					}
					n.heldApp <- command
					connection, _, err := w.(http.Hijacker).Hijack()
					if err == nil {
						connection.Close()
					}
					return
				}
				if n.dropApp.Load() && r.URL.Path == coordination.RPCPath+"app" {
					recorder := httptest.NewRecorder()
					handler.ServeHTTP(recorder, r)
					if recorder.Code == http.StatusOK {
						connection, _, err := w.(http.Hijacker).Hijack()
						if err == nil {
							connection.Close()
						}
						return
					}
					w.WriteHeader(recorder.Code)
					_, _ = io.Copy(w, recorder.Body)
					return
				}
				handler.ServeHTTP(w, r)
				return
			}
			http.Error(w, "replica stopped", http.StatusServiceUnavailable)
		}))
		n.server.TLS, err = n.options.ServerConfig()
		if err != nil {
			t.Fatal(err)
		}
		n.server.StartTLS()
		nodes[i] = n
		members = append(members, coordination.Member{NodeID: id, Address: n.listener.Addr().String(), APIAddress: n.server.URL, AutoEligible: true})
	}
	for i, n := range nodes {
		var err error
		n.options.AuthorizePeer = func(identity coordination.Identity) bool {
			if identity.ClusterID != "test-cluster" {
				return false
			}
			if current := n.runtime.Load(); current != nil {
				peers := current.TransportPeers()
				if len(peers) > 0 {
					_, ok := peers[identity.NodeID]
					return ok
				}
			}
			for _, member := range members {
				if member.NodeID == identity.NodeID {
					return true
				}
			}
			return false
		}
		n.client, err = coordination.NewClient(coordination.ClientConfig{TLS: n.options, Members: members, Timeout: time.Second, RetryWindow: timing.retryWindow, ControlHeaders: func(context.Context, string) (http.Header, error) {
			return http.Header{"X-Test-Owner": []string{"owner"}}, nil
		}})
		if err != nil {
			t.Fatal(err)
		}
		stream, err := coordination.NewTLSStreamLayer(n.listener, n.options, n.client.PeerID, nil)
		if err != nil {
			t.Fatal(err)
		}
		raftConfig := raft.DefaultConfig()
		raftConfig.HeartbeatTimeout = timing.heartbeat
		raftConfig.ElectionTimeout = timing.election
		raftConfig.LeaderLeaseTimeout = timing.lease
		raftConfig.CommitTimeout = 10 * time.Millisecond
		n.config = Config{LedgerDir: filepath.Join(t.TempDir(), "ledger"), Client: n.client, PollInterval: 20 * time.Millisecond, ShutdownTimeout: time.Second,
			Coordination: coordination.Config{ClusterID: "test-cluster", NodeID: members[i].NodeID, FailureDomain: "test-domain-" + members[i].NodeID, StorageLevel: "restricted", DataDir: filepath.Join(t.TempDir(), "raft"), Bootstrap: i == 0,
				APIAddress: n.server.URL, StreamLayer: stream, RaftConfig: raftConfig, ApplyTimeout: 3 * time.Second, ProbeInterval: 500 * time.Millisecond, FailoverTimeout: time.Second}}
		n.config.Activate = func(ctx context.Context, activation Activation) (Deactivate, error) {
			sessions, err := statepkg.OpenLedger(activation.Ledger)
			if err != nil {
				return nil, err
			}
			tasks, err := task.OpenLedger(activation.Ledger)
			if err != nil {
				return nil, err
			}
			stores := &businessStores{activation: activation, state: sessions, tasks: tasks}
			n.mu.Lock()
			n.business = stores
			n.activated = append(n.activated, stores)
			n.mu.Unlock()
			return func(context.Context) error {
				if ctx.Err() == nil {
					return errors.New("business stopped before activation context was canceled")
				}
				n.mu.Lock()
				n.business = nil
				n.stopped++
				n.mu.Unlock()
				return nil
			}, nil
		}
	}
	t.Cleanup(func() {
		for _, n := range nodes {
			if current := n.runtime.Swap(nil); current != nil {
				if err := current.Close(); err != nil {
					t.Error(err)
				}
			}
			n.server.Close()
			n.client.Close()
			n.listener.Close()
		}
	})
	return nodes
}

func TestLateProposalCannotCommitAfterBusinessCachesAreReconstructed(t *testing.T) {
	nodes := testNodes(t, 2)
	first := openNode(t, nodes[0])
	ready(t, first)
	second := joinNode(t, first, nodes[1], true, false)
	if _, err := first.Transfer(t.Context(), coordination.TransferRequest{ID: "transfer", Actor: "owner", ExpectedEpoch: 1, TargetNodeID: "node-2"}); err != nil {
		t.Fatal(err)
	}
	old := ready(t, second)
	nodes[0].holdApp.Store(true)
	err := nodes[1].current().state.SetActiveAgent("late-original", "worker")
	nodes[0].holdApp.Store(false)
	if err == nil {
		t.Fatal("held proposal unexpectedly succeeded")
	}
	var late coordination.AppCommand
	select {
	case late = <-nodes[0].heldApp:
	case <-time.After(time.Second):
		t.Fatal("proposal was not captured")
	}
	fresh := ready(t, second)
	if fresh.WriterGeneration <= old.WriterGeneration {
		t.Fatal("new activation did not advance the durable writer fence")
	}
	if _, err := first.service.ApplyApp(t.Context(), late); !errors.Is(err, coordination.ErrStaleWriter) {
		t.Fatalf("late old-generation proposal crossed the activation fence: %v", err)
	}
	if err := nodes[1].current().state.SetActiveAgent("subsequent", "worker"); err != nil {
		t.Fatal(err)
	}
}

func TestUnknownProposalOutcomeRevokesCachedStoresBeforeAnotherWrite(t *testing.T) {
	nodes := testNodes(t, 2)
	first := openNode(t, nodes[0])
	ready(t, first)
	second := joinNode(t, first, nodes[1], true, false)
	if _, err := first.Transfer(t.Context(), coordination.TransferRequest{ID: "transfer", Actor: "owner", ExpectedEpoch: 1, TargetNodeID: "node-2"}); err != nil {
		t.Fatal(err)
	}
	active := ready(t, second)
	old := nodes[1].current()
	if !first.Status().IsLeader {
		t.Fatal("fixture no longer routes business writes through the remote consensus leader")
	}
	nodes[0].dropApp.Store(true)
	err := old.state.SetActiveAgent("committed-without-response", "worker")
	nodes[0].dropApp.Store(false)
	if err == nil {
		t.Fatal("lost commit response was acknowledged as success")
	}
	if active.Context.Err() == nil {
		t.Fatal("unknown commit result left the cached store authorized")
	}
	if err := old.state.SetActiveAgent("stale-overwrite", "worker"); err == nil {
		t.Fatal("unknown commit result allowed a later stale document replacement")
	}
	newActive := ready(t, second)
	if newActive.Generation == active.Generation {
		t.Fatal("unknown commit result did not create a fresh business generation")
	}
	if got := nodes[1].current().state.Conversation("committed-without-response").ActiveAgent; got != "worker" {
		t.Fatal("reconstructed store lost the write whose response disappeared")
	}
}

func TestCallerCancellationDuringProposalKeepsBusinessGeneration(t *testing.T) {
	nodes := testNodes(t, 2)
	first := openNode(t, nodes[0])
	ready(t, first)
	second := joinNode(t, first, nodes[1], true, false)
	if _, err := first.Transfer(t.Context(), coordination.TransferRequest{ID: "transfer", Actor: "owner", ExpectedEpoch: 1, TargetNodeID: "node-2"}); err != nil {
		t.Fatal(err)
	}
	active := ready(t, second)
	if !first.Status().IsLeader {
		t.Fatal("fixture no longer routes business writes through the remote consensus leader")
	}
	gate := &appGate{arrived: make(chan struct{}), release: make(chan struct{})}
	nodes[0].gateApp.Store(gate)
	caller, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- active.Ledger.PutBinding(caller, "test", "caller-canceled", "committed") }()
	select {
	case <-gate.arrived:
	case <-time.After(8 * time.Second):
		t.Fatal("proposal did not reach the consensus leader")
	}
	// The caller gives up while its proposal is in flight, as a turn does
	// when its prompt timeout expires during a ledger write.
	cancel()
	close(gate.release)
	err := <-done
	nodes[0].gateApp.Store(nil)
	if active.Context.Err() != nil {
		t.Fatalf("caller cancellation revoked business generation %d: %v", active.Generation, second.Status().LastError)
	}
	if err != nil {
		t.Fatalf("committed write was reported as failed: %v", err)
	}
	var got string
	if ok, err := active.Ledger.GetBinding(t.Context(), "test", "caller-canceled", &got); err != nil || !ok || got != "committed" {
		t.Fatalf("write not applied locally: ok=%v value=%q err=%v", ok, got, err)
	}
	if err := active.Ledger.PutBinding(t.Context(), "test", "next", "written"); err != nil {
		t.Fatalf("generation cannot write after a caller cancellation: %v", err)
	}
}

// A control command this member answers as unavailable goes to the member
// that leads next. Here this member's consensus replica has stopped; a
// leadership lost part-way through a command is answered the same way.
func TestJoinThisMemberCannotTakeGoesToTheNextLeader(t *testing.T) {
	nodes := testNodes(t, 4)
	first := openNode(t, nodes[0])
	ready(t, first)
	for _, n := range nodes[1:3] {
		joinNode(t, first, n, true, false)
	}
	if err := first.service.Close(); err != nil {
		t.Fatal(err)
	}
	// The runtime stops with its replica and reports that when closed.
	nodes[0].runtime.Store(nil)
	t.Cleanup(func() { _ = first.Close() })
	fourth := openNode(t, nodes[3])
	member := coordination.Member{NodeID: "node-4", Address: fourth.Status().Address, APIAddress: nodes[3].server.URL}
	if _, err := first.Join(t.Context(), coordination.JoinRequest{ID: "join-node-4", Actor: "owner", Member: member}); err != nil {
		t.Fatalf("a join this member could not take did not reach the next leader: %v", err)
	}
	state, err := nodes[1].runtime.Load().ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := state.Members["node-4"]; !ok {
		t.Fatalf("node-4 is not a member after its join: %+v", state.Members)
	}
}

// A leader that cannot finish a control command answers with its own error.
// The client routes a command to the leader, so forwarding it would send the
// command back to this member again and again until the client's retry
// window ran out, and hide what went wrong behind that window. Here node-1
// removes itself and cannot hand its leadership to node-2, which receives no
// Raft traffic.
func TestControlCommandTheLeaderCannotFinishIsNotForwardedToItself(t *testing.T) {
	nodes := testNodesWith(t, 3, steadyTiming)
	first := openNode(t, nodes[0])
	ready(t, first)
	for _, n := range nodes[1:] {
		joinNode(t, first, n, true, false)
	}
	second, third := nodes[1].runtime.Load(), nodes[2].runtime.Load()
	// A member that coordinates cannot be removed.
	if _, err := first.Transfer(t.Context(), coordination.TransferRequest{ID: "transfer", Actor: "owner", ExpectedEpoch: 1, TargetNodeID: "node-3"}); err != nil {
		t.Fatal(err)
	}
	ready(t, third)
	// node-1 hands its leadership only to a member that has applied what it
	// has; node-2 is the first such member it tries.
	deadline := time.Now().Add(8 * time.Second)
	for second.service.Status().AppliedIndex < first.service.Status().AppliedIndex {
		if time.Now().After(deadline) {
			t.Fatalf("node-2 did not catch up with node-1: %d < %d", second.service.Status().AppliedIndex, first.service.Status().AppliedIndex)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if status := first.service.Status(); !status.IsLeader {
		t.Fatalf("node-1 is to remove itself as the consensus leader, but the leader is %q", status.LeaderID)
	}
	nodes[1].raft.pause()
	t.Cleanup(nodes[1].raft.resume)
	_, err := first.Remove(t.Context(), coordination.RemoveRequest{ID: "remove-node-1", Actor: "owner", NodeID: "node-1"})
	if !errors.Is(err, coordination.ErrUnavailable) || !strings.Contains(err.Error(), "consensus leadership transfer to node-2") {
		t.Fatalf("node-1 did not answer with the leadership transfer it could not finish: %v", err)
	}
	for _, n := range nodes {
		if got := n.servedCount(coordination.RPCPath + "remove"); got != 0 {
			t.Fatalf("%s received the removal %d times; the leader forwarded a command only it could take", n.config.Coordination.NodeID, got)
		}
	}
}

// A leader that loses its leadership while it runs a control command no
// longer leads when the command fails, so the command goes, with its ID, to
// the member elected next. Here node-1 stops hearing from both other members
// once the command has started, and steps down when its lease runs out.
func TestControlCommandWhoseLeaderStepsDownPartWayGoesToTheNextLeader(t *testing.T) {
	nodes := testNodesWith(t, 3, steadyTiming)
	first := openNode(t, nodes[0])
	ready(t, first)
	for _, n := range nodes[1:] {
		joinNode(t, first, n, true, false)
	}
	state, err := first.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if status := first.service.Status(); !status.IsLeader {
		t.Fatalf("node-1 is to lose its leadership part-way, but the leader is %q", status.LeaderID)
	}
	for _, n := range nodes[1:] {
		n.raft.pause()
		t.Cleanup(n.raft.resume)
	}
	done := make(chan error, 1)
	go func() {
		_, err := first.Rename(t.Context(), coordination.RenameRequest{ID: "rename-node-3", Actor: "owner", ExpectedRevision: state.Revision, NodeID: "node-3", Name: "third"})
		done <- err
	}()
	deadline := time.Now().Add(10 * time.Second)
	for first.service.Status().IsLeader {
		if time.Now().After(deadline) {
			t.Fatal("node-1 kept its leadership without hearing from a quorum")
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, n := range nodes[1:] {
		n.raft.resume()
	}
	select {
	case err = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the rename did not finish after node-1 stepped down")
	}
	if err != nil {
		t.Fatalf("a rename whose leader stepped down part-way did not reach the next leader: %v", err)
	}
	deadline = time.Now().Add(8 * time.Second)
	for first.MemberNames()["node-3"] != "third" {
		if time.Now().After(deadline) {
			t.Fatalf("node-3 is named %q on node-1 after its rename", first.MemberNames()["node-3"])
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Which answers from this member send a control command on to the member
// the client finds leading. A member that no longer leads, because it lost
// leadership part-way, its replica stopped or its service closed, forwards an
// unavailable or failed-replica answer; a member that still leads answers with
// its own. The service closes itself as soon as its replica fails, so this
// checks the decision directly rather than racing that closure.
func TestControlCommandForwardingDependsOnWhetherThisMemberStillLeads(t *testing.T) {
	nodes := testNodes(t, 1)
	r := openNode(t, nodes[0])
	ready(t, r)
	check := func(situation string, want map[error]bool) {
		t.Helper()
		for kind, forward := range want {
			if got := r.forwardable(fmt.Errorf("%w: from the test", kind)); got != forward {
				t.Errorf("%s, forwarding %v is %v, want %v", situation, kind, got, forward)
			}
		}
	}
	if !r.service.Status().IsLeader {
		t.Fatal("the single member does not lead consensus")
	}
	check("while this member leads", map[error]bool{coordination.ErrNotLeader: true, coordination.ErrUnavailable: false, coordination.ErrApplication: false, coordination.ErrInvalid: false, coordination.ErrConflict: false})
	nodes[0].runtime.Store(nil)
	t.Cleanup(func() { _ = r.Close() })
	if err := r.service.Close(); err != nil {
		t.Fatal(err)
	}
	check("once this member's service closed", map[error]bool{coordination.ErrNotLeader: true, coordination.ErrUnavailable: true, coordination.ErrApplication: true, coordination.ErrInvalid: false, coordination.ErrConflict: false})
}

func TestCommittedWriteWhoseLocalApplyStallsRevokesBusinessGeneration(t *testing.T) {
	nodes := testNodesWith(t, 3, steadyTiming)
	first := openNode(t, nodes[0])
	ready(t, first)
	for _, n := range nodes[1:] {
		joinNode(t, first, n, true, false)
	}
	second := nodes[1].runtime.Load()
	if _, err := first.Transfer(t.Context(), coordination.TransferRequest{ID: "transfer", Actor: "owner", ExpectedEpoch: 1, TargetNodeID: "node-2"}); err != nil {
		t.Fatal(err)
	}
	active := ready(t, second)
	requireAnotherLeader(t, first, "node-2")
	// node-2 stops receiving Raft traffic. node-1 and node-3 still form a
	// quorum, so the write commits but never reaches node-2's ledger.
	nodes[1].raft.pause()
	t.Cleanup(nodes[1].raft.resume)
	// The caller has no deadline and never cancels: only ApplyTimeout
	// bounds the wait for the committed write to reach the local ledger.
	done := make(chan error, 1)
	go func() { done <- active.Ledger.PutBinding(context.Background(), "test", "applied-late", "committed") }()
	var err error
	select {
	case err = <-done:
	case <-time.After(4 * nodes[1].config.Coordination.ApplyTimeout):
		t.Fatal("the write kept waiting for a local apply that cannot happen")
	}
	if err == nil {
		t.Fatal("a write not applied locally was reported as applied")
	}
	if active.Context.Err() == nil {
		t.Fatal("an unconfirmed local apply left the business generation authorized")
	}
	leader, err := first.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if local, err := second.Ledger().ReplicaVersion(); err != nil || local >= leader.AppVersion {
		t.Fatalf("the write reached node-2 before its Raft traffic resumed: local=%d committed=%d err=%v", local, leader.AppVersion, err)
	}
	nodes[1].raft.resume()
	fresh := ready(t, second)
	if fresh.Generation == active.Generation {
		t.Fatal("the stalled apply did not lead to a fresh business generation")
	}
	var got string
	if ok, err := fresh.Ledger.GetBinding(t.Context(), "test", "applied-late", &got); err != nil || !ok || got != "committed" {
		t.Fatalf("reconstructed generation lost the committed write: ok=%v value=%q err=%v", ok, got, err)
	}
}

// A write checks, before it proposes anything, that this replica has caught
// up with a linearizable read. The task store holds its lock and the ledger
// its writer lock through that check, so a replica that stays behind must
// fail the write instead of stalling every writer queued behind it.
func TestWriteOnAReplicaThatCannotCatchUpFailsAsUnavailable(t *testing.T) {
	nodes := testNodesWith(t, 3, steadyTiming)
	// node-2's application writes while its generation is being activated.
	// The runtime then waits for Activate and does not check the replica's
	// progress itself, so nothing but the write's own check bounds the wait.
	activating := make(chan Activation, 1)
	proceed := make(chan struct{})
	release := sync.OnceFunc(func() { close(proceed) })
	t.Cleanup(release)
	build := nodes[1].config.Activate
	nodes[1].config.Activate = func(ctx context.Context, activation Activation) (Deactivate, error) {
		stop, err := build(ctx, activation)
		if err == nil {
			select {
			case activating <- activation:
			default:
			}
			select {
			case <-proceed:
			case <-ctx.Done():
			}
		}
		return stop, err
	}
	first := openNode(t, nodes[0])
	ready(t, first)
	for _, n := range nodes[1:] {
		joinNode(t, first, n, true, false)
	}
	second := nodes[1].runtime.Load()
	if _, err := first.Transfer(t.Context(), coordination.TransferRequest{ID: "transfer", Actor: "owner", ExpectedEpoch: 1, TargetNodeID: "node-2"}); err != nil {
		t.Fatal(err)
	}
	var active Activation
	select {
	case active = <-activating:
	case <-time.After(8 * time.Second):
		t.Fatalf("node-2 did not start its business generation: %+v", second.Status())
	}
	tasks := nodes[1].current().tasks
	requireAnotherLeader(t, first, "node-2")
	// node-2 stops receiving Raft traffic, and node-1 and node-3 commit an
	// entry it cannot apply: a read from the leader is now ahead of node-2.
	nodes[1].raft.pause()
	t.Cleanup(nodes[1].raft.resume)
	state, err := first.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Rename(t.Context(), coordination.RenameRequest{ID: "rename-third", Actor: "owner", ExpectedRevision: state.Revision, NodeID: "node-3", Name: "third"}); err != nil {
		t.Fatal(err)
	}
	stored := len(tasks.List(""))
	pending := task.Task{Channel: "replica-wait", Member: "owner", Goal: "written once node-2 catches up"}
	applyTimeout := nodes[1].config.Coordination.ApplyTimeout
	done := make(chan error, 1)
	// Create writes without a deadline, as every task store write does.
	go func() { _, err := tasks.Create(pending); done <- err }()
	select {
	case err = <-done:
	case <-time.After(2 * applyTimeout):
		t.Fatalf("the write kept waiting for a replica that cannot catch up; ApplyTimeout is %s", applyTimeout)
	}
	if !errors.Is(err, coordination.ErrUnavailable) {
		t.Fatalf("a replica that did not catch up failed the write with %v, not as unavailable", err)
	}
	if active.Context.Err() != nil {
		t.Fatalf("a write that proposed nothing revoked business generation %d: %v", active.Generation, second.Status().LastError)
	}
	if got := len(tasks.List("")); got != stored {
		t.Fatalf("the task store kept a write that failed: %d tasks, had %d", got, stored)
	}
	// A caller whose own deadline ends before ApplyTimeout gets that deadline
	// back, not a report that the replica is unavailable.
	short, cancel := context.WithTimeout(t.Context(), applyTimeout/3)
	err = active.Ledger.PutBinding(short, "test", "caller-deadline", "never")
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, coordination.ErrUnavailable) {
		t.Fatalf("a write whose caller's deadline ended first failed with %v, not with that deadline", err)
	}
	nodes[1].raft.resume()
	if _, err := tasks.Create(pending); err != nil {
		t.Fatalf("the same write failed again after node-2 could catch up: %v", err)
	}
	release()
	if published := ready(t, second); published.Generation != active.Generation {
		t.Fatalf("generation %d was published instead of generation %d, whose write failed", published.Generation, active.Generation)
	}
}

// Activation commits a writer fence and waits for this replica to apply it
// before building any store. The runtime loop runs activation itself, so if
// nothing bounded that wait the node would stay silently stuck, unable to
// notice a lost assignment or try again.
func TestActivationWhoseWriterFenceCannotApplyGivesUpAndRetries(t *testing.T) {
	nodes := testNodes(t, 3)
	first := openNode(t, nodes[0])
	ready(t, first)
	for _, n := range nodes[1:] {
		joinNode(t, first, n, true, false)
	}
	second := nodes[1].runtime.Load()
	gate := &appGate{arrived: make(chan struct{}), release: make(chan struct{})}
	nodes[0].gateFence.Store(gate)
	release := sync.OnceFunc(func() { close(gate.release) })
	t.Cleanup(release)
	if _, err := first.Transfer(t.Context(), coordination.TransferRequest{ID: "transfer", Actor: "owner", ExpectedEpoch: 1, TargetNodeID: "node-2"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gate.arrived:
	case <-time.After(8 * time.Second):
		t.Fatalf("node-2 did not ask for a writer fence: %+v", second.Status())
	}
	second.mu.Lock()
	stalled := second.current
	second.mu.Unlock()
	if stalled == nil {
		t.Fatal("node-2 asked for a writer fence without a generation being activated")
	}
	if !first.Status().IsLeader {
		t.Fatal("fixture no longer commits node-2's writer fence on a remote consensus leader")
	}
	// node-2 stops receiving Raft traffic before its fence is committed:
	// node-1 and node-3 commit it, and node-2 cannot apply it.
	nodes[1].raft.pause()
	t.Cleanup(nodes[1].raft.resume)
	release()
	nodes[0].gateFence.Store(nil)
	applyTimeout := nodes[1].config.Coordination.ApplyTimeout
	select {
	case <-stalled.Context.Done():
	case <-time.After(2 * applyTimeout):
		t.Fatalf("activation of generation %d kept waiting for a writer fence node-2 cannot apply; ApplyTimeout is %s", stalled.Generation, applyTimeout)
	}
	second.mu.Lock()
	cause := second.lastError
	second.mu.Unlock()
	if !errors.Is(cause, coordination.ErrUnavailable) {
		t.Fatalf("activation gave up without reporting that the replica is behind: %v", cause)
	}
	nodes[1].raft.resume()
	fresh := ready(t, second)
	if fresh.Generation <= stalled.Generation {
		t.Fatalf("generation %d was published, not a new attempt after generation %d", fresh.Generation, stalled.Generation)
	}
	if err := fresh.Ledger.PutBinding(t.Context(), "test", "after-fence", "written"); err != nil {
		t.Fatalf("the generation activated after the stall cannot write: %v", err)
	}
}

func TestCallerCancellationBeforeProposalSubmitsNothing(t *testing.T) {
	nodes := testNodes(t, 1)
	r := openNode(t, nodes[0])
	active := ready(t, r)
	caller, cancel := context.WithCancel(t.Context())
	cancel()
	if err := active.Ledger.PutBinding(caller, "test", "never", "written"); !errors.Is(err, context.Canceled) {
		t.Fatalf("write under a canceled caller: %v", err)
	}
	if active.Context.Err() != nil {
		t.Fatal("a write that was never proposed revoked the business generation")
	}
	var got string
	if ok, err := active.Ledger.GetBinding(t.Context(), "test", "never", &got); err != nil || ok {
		t.Fatalf("canceled write was applied: ok=%v err=%v", ok, err)
	}
}

func TestReplicaRestoreCancelsAndReconstructsBusinessGeneration(t *testing.T) {
	nodes := testNodes(t, 1)
	r := openNode(t, nodes[0])
	first := ready(t, r)
	old := nodes[0].current()
	if err := old.state.SetActiveAgent("original", "codex"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := (application{runtime: r}).Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if err := (application{runtime: r}).Restore(snapshot); err != nil {
		t.Fatal(err)
	}
	if first.Context.Err() == nil {
		t.Fatal("snapshot restore did not synchronously revoke the active generation")
	}
	next := ready(t, r)
	if next.Generation == first.Generation || next.Ledger == first.Ledger {
		t.Fatal("snapshot restore retained the cached business generation")
	}
	if err := old.state.SetActiveAgent("stale", "other"); err == nil {
		t.Fatal("restored generation accepted an old cached store write")
	}
	if got := nodes[0].current().state.Conversation("original").ActiveAgent; got != "codex" {
		t.Fatal("activation did not reload restored session data")
	}
}

func TestCloseIsBoundedAndKeepsLedgerOpenUntilBusinessStops(t *testing.T) {
	nodes := testNodes(t, 1)
	release := make(chan struct{})
	started := make(chan struct{})
	nodes[0].config.ShutdownTimeout = 80 * time.Millisecond
	nodes[0].config.Activate = func(ctx context.Context, activation Activation) (Deactivate, error) {
		return func(context.Context) error {
			if ctx.Err() == nil {
				t.Error("shutdown did not cancel the activation context first")
			}
			close(started)
			<-release
			return nil
		}, nil
	}
	r := openNode(t, nodes[0])
	active := ready(t, r)
	if err := active.Ledger.Document("keep-open").Save([]byte("value")); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := r.Close(); !errors.Is(err, ErrShutdownTimeout) {
		t.Fatalf("shutdown timeout was not exposed: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("Close blocked on an uncooperative business callback")
	}
	<-started
	if raw, ok, err := r.Ledger().Document("keep-open").Load(); err != nil || !ok || string(raw) != "value" {
		t.Fatalf("ledger closed before business termination: %s %v %v", raw, ok, err)
	}
	if err := active.Ledger.Document("stale").Save([]byte("unsafe")); err == nil {
		t.Fatal("timed-out shutdown left a writable business generation")
	}
	close(release)
	select {
	case <-r.closeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not finish after business released its ledger")
	}
	nodes[0].runtime.Store(nil)
}

// A local application that cannot be built is this node's own trouble: the
// replica keeps replicating and voting, says why it is not ready, and builds
// again on the next attempt instead of dropping out of the cluster.
func TestActivationFailureKeepsTheReplicaAndBuildsAgain(t *testing.T) {
	nodes := testNodes(t, 1)
	failure := errors.New("cannot rebuild business stores")
	build := nodes[0].config.Activate
	var attempts atomic.Int64
	var observed Status
	nodes[0].config.Activate = func(ctx context.Context, activation Activation) (Deactivate, error) {
		switch attempts.Add(1) {
		case 1:
			return nil, failure
		case 2:
			observed = activation.Runtime.Status()
			return nil, failure
		}
		return build(ctx, activation)
	}
	r := openNode(t, nodes[0])
	active := ready(t, r)
	if observed.Closed || !observed.Healthy || !strings.Contains(observed.LastError, failure.Error()) {
		t.Fatalf("a failed build did not leave a healthy replica: %+v", observed)
	}
	if err := active.Ledger.Document("rebuilt").Save([]byte("value")); err != nil {
		t.Fatalf("the generation that finally activated cannot write: %v", err)
	}
	if status := r.Status(); !status.Ready || status.Closed || status.LastError != "" {
		t.Fatalf("a recovered runtime still reports its earlier failure: %+v", status)
	}
}

func TestGenerationFailureIsNonBlockingAndIgnoresEarlierInstances(t *testing.T) {
	nodes := testNodes(t, 1)
	release := make(chan struct{})
	nodes[0].config.Activate = func(context.Context, Activation) (Deactivate, error) {
		return func(context.Context) error { <-release; return nil }, nil
	}
	r := openNode(t, nodes[0])
	active := ready(t, r)
	failure := errors.New("application server exited")
	r.FailGeneration(active.Generation-1, failure)
	if !r.Status().Ready || r.Status().Closed {
		t.Fatal("earlier instance failure stopped the current generation")
	}
	finished := make(chan struct{})
	go func() { r.FailGeneration(active.Generation, failure); close(finished) }()
	select {
	case <-finished:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("failure reporting waited for application termination")
	}
	if !r.Status().Closed || active.Context.Err() == nil {
		t.Fatal("current application failure did not revoke its generation")
	}
	close(release)
	nodes[0].runtime.Store(nil)
	if err := r.Close(); !errors.Is(err, failure) {
		t.Fatalf("application failure was lost: %v", err)
	}
}

func TestRestoreDuringActivationRebuildsInsteadOfPublishingCanceledStores(t *testing.T) {
	nodes := testNodes(t, 1)
	started := make(chan struct{})
	var calls atomic.Int64
	nodes[0].config.Activate = func(ctx context.Context, activation Activation) (Deactivate, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return nil, nil
	}
	r := openNode(t, nodes[0])
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first activation did not start")
	}
	snapshot, err := (application{runtime: r}).Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if err := (application{runtime: r}).Restore(snapshot); err != nil {
		t.Fatal(err)
	}
	active := ready(t, r)
	if active.Generation != 2 || calls.Load() != 2 {
		t.Fatalf("restore published an obsolete activation: generation=%d calls=%d", active.Generation, calls.Load())
	}
}

func TestTransferCancelsAnActivationThatHasNotFinishedStarting(t *testing.T) {
	nodes := testNodes(t, 2)
	started := make(chan context.Context, 1)
	nodes[0].config.Activate = func(ctx context.Context, activation Activation) (Deactivate, error) {
		started <- ctx
		<-ctx.Done()
		return nil, ctx.Err()
	}
	first := openNode(t, nodes[0])
	var active context.Context
	select {
	case active = <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("business activation did not start")
	}
	second := joinNode(t, first, nodes[1], true, false)
	if _, err := first.Transfer(t.Context(), coordination.TransferRequest{ID: "transfer-during-startup", Actor: "owner", ExpectedEpoch: 1, TargetNodeID: "node-2"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-active.Done():
	case <-time.After(500 * time.Millisecond):
		t.Fatal("unfinished activation was not canceled after coordinator transfer")
	}
	ready(t, second)
	if first.Status().Ready {
		t.Fatal("unfinished old generation was published")
	}
}

func openNode(t *testing.T, n *clusterNode) *Runtime {
	t.Helper()
	r, err := Open(n.config)
	if err != nil {
		t.Fatal(err)
	}
	n.runtime.Store(r)
	return r
}

// joinNode opens n and has first admit it under the command ID
// "join-<node ID>", with or without a Raft vote and automatic coordinator
// eligibility.
func joinNode(t *testing.T, first *Runtime, n *clusterNode, voting, autoEligible bool) *Runtime {
	t.Helper()
	r := openNode(t, n)
	member := coordination.Member{NodeID: r.Status().NodeID, Address: r.Status().Address, APIAddress: n.server.URL, AutoEligible: autoEligible, Voting: voting}
	if _, err := first.Join(t.Context(), coordination.JoinRequest{ID: "join-" + member.NodeID, Actor: "owner", Member: member}); err != nil {
		t.Fatal(err)
	}
	return r
}

// requireAnotherLeader fails the test unless a member other than paused
// leads consensus. A test pauses that member's inbound Raft traffic next:
// the other two members still form a quorum and commit what it cannot
// receive. Were paused the leader, its own replication would carry on.
func requireAnotherLeader(t *testing.T, r *Runtime, paused string) {
	t.Helper()
	if leader := r.service.Status().LeaderID; leader == "" || leader == paused {
		t.Fatalf("%s is to stop receiving Raft traffic while another member leads consensus, but the leader is %q", paused, leader)
	}
}

func ready(t *testing.T, r *Runtime) Activation {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	activation, err := r.WaitReady(ctx)
	if err != nil {
		t.Fatalf("runtime did not activate: %v, status=%+v", err, r.Status())
	}
	return activation
}

// A quorum read appends a barrier entry to the Raft log, and from a member
// that does not lead it is a request to the leader, over what may be a slow
// link, for the whole cluster state. Once the business generation is ready
// and nothing changes, no node's runtime keeps reading the state that way:
// the log stops growing and no member asks the leader for the state.
func TestIdleRuntimesAppendNothingAndAskTheLeaderForNothing(t *testing.T) {
	nodes := testNodes(t, 2)
	first := openNode(t, nodes[0])
	ready(t, first)
	second := openNode(t, nodes[1])
	member := coordination.Member{NodeID: "node-2", Address: second.Status().Address, APIAddress: nodes[1].server.URL}
	if _, err := first.Join(t.Context(), coordination.JoinRequest{ID: "join-replica", Actor: "owner", Member: member}); err != nil {
		t.Fatal(err)
	}
	ready(t, first)
	idle := 25 * nodes[0].config.PollInterval
	index, reads := first.service.LastIndex(), nodes[0].servedCount(coordination.RPCPath+"state")
	time.Sleep(idle)
	if grown := first.service.LastIndex() - index; grown != 0 {
		t.Errorf("the Raft log grew by %d entries while the cluster was idle for %s", grown, idle)
	}
	if asked := nodes[0].servedCount(coordination.RPCPath+"state") - reads; asked != 0 {
		t.Errorf("the leader was asked for the cluster state %d times while the cluster was idle for %s", asked, idle)
	}
	if status := first.Status(); !status.Ready {
		t.Fatalf("the coordinator stopped being ready while idle: %+v", status)
	}
}

// A coordinator that hears from no consensus leader for longer than
// ApplyTimeout gives up its business generation, although nothing is
// written: it can no longer tell whether it still holds its role.
func TestCoordinatorThatHearsFromNoLeaderGivesUpItsGeneration(t *testing.T) {
	nodes := testNodesWith(t, 3, steadyTiming)
	first := openNode(t, nodes[0])
	ready(t, first)
	for i := 1; i < len(nodes); i++ {
		r := openNode(t, nodes[i])
		member := coordination.Member{NodeID: r.Status().NodeID, Address: r.Status().Address, APIAddress: nodes[i].server.URL, Voting: true}
		if _, err := first.Join(t.Context(), coordination.JoinRequest{ID: "join-" + member.NodeID, Actor: "owner", Member: member}); err != nil {
			t.Fatal(err)
		}
	}
	second := nodes[1].runtime.Load()
	if _, err := first.Transfer(t.Context(), coordination.TransferRequest{ID: "transfer", Actor: "owner", ExpectedEpoch: 1, TargetNodeID: "node-2"}); err != nil {
		t.Fatal(err)
	}
	active := ready(t, second)
	requireAnotherLeader(t, first, "node-2")
	// node-2 stops receiving Raft traffic, so it hears from no leader.
	nodes[1].raft.pause()
	t.Cleanup(nodes[1].raft.resume)
	applyTimeout := nodes[1].config.Coordination.ApplyTimeout
	select {
	case <-active.Context.Done():
	case <-time.After(3 * applyTimeout):
		t.Fatalf("a coordinator that heard from no leader for %s kept business generation %d: %+v", 3*applyTimeout, active.Generation, second.Status())
	}
	second.mu.Lock()
	cause := second.lastError
	second.mu.Unlock()
	if !errors.Is(cause, coordination.ErrUnavailable) {
		t.Fatalf("the generation was given up without reporting that no leader was heard from: %v", cause)
	}
	nodes[1].raft.resume()
	if fresh := ready(t, second); fresh.Generation <= active.Generation {
		t.Fatalf("generation %d was published, not a new one after generation %d", fresh.Generation, active.Generation)
	}
}

func TestThreeNodeLedgerTransfersPreserveFactsAndFenceEveryOldGeneration(t *testing.T) {
	nodes := testNodes(t, 3)
	// Existing version-zero facts are absent from the Raft log. A new node
	// must receive the complete SQLite snapshot before incremental commands.
	baseline, err := ledger.Open(nodes[0].config.LedgerDir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := baseline.Document("existing-installation").Save([]byte(`{"workspace":"original"}`)); err != nil {
		t.Fatal(err)
	}
	if err := baseline.Close(); err != nil {
		t.Fatal(err)
	}
	first := openNode(t, nodes[0])
	initial := ready(t, first)
	if err := first.Ledger().Document("bypass").Save([]byte("bad")); err == nil {
		t.Fatal("FSM ledger accepted an unfenced business write")
	}
	stores := nodes[0].current()
	if err := stores.state.SetActiveAgent("session-1", "codex"); err != nil {
		t.Fatal(err)
	}
	created, err := stores.tasks.Create(task.Task{Channel: "session-1", Member: "codex", Goal: "continue after transfer"})
	if err != nil {
		t.Fatal(err)
	}
	if err := initial.Ledger.Document("console").Save([]byte(`{"session-1":{"prompt":"original"}}`)); err != nil {
		t.Fatal(err)
	}
	lease, err := initial.Ledger.Acquire(t.Context(), "workspace", "attempt-1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	before, err := first.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes[1:] {
		r := joinNode(t, first, n, true, true)
		if version, err := r.Ledger().ReplicaVersion(); err != nil || version < before.AppVersion {
			t.Fatalf("joining node lacks complete baseline: %d, %v", version, err)
		}
		if raw, ok, err := r.Ledger().Document("console").Load(); err != nil || !ok || string(raw) != `{"session-1":{"prompt":"original"}}` {
			t.Fatalf("snapshot omitted document: %s %v %v", raw, ok, err)
		}
		if raw, ok, err := r.Ledger().Document("existing-installation").Load(); err != nil || !ok || string(raw) != `{"workspace":"original"}` {
			t.Fatalf("joining replica omitted version-zero baseline: %s %v %v", raw, ok, err)
		}
	}
	second := nodes[1].runtime.Load()
	if _, err := first.Transfer(t.Context(), coordination.TransferRequest{ID: "to-node-2", Actor: "owner", ExpectedEpoch: 1, TargetNodeID: "node-2", Reason: "user"}); err != nil {
		t.Fatal(err)
	}
	transferred := ready(t, second)
	if transferred.Assignment.Epoch != 2 || transferred.Generation == 0 {
		t.Fatal("new coordinator did not bind its generation")
	}
	next := nodes[1].current()
	if next.state.Conversation("session-1").ActiveAgent != "codex" {
		t.Fatal("session state was lost")
	}
	if got, ok := next.tasks.Get(created.ID); !ok || got.Goal != created.Goal {
		t.Fatal("task state was lost")
	}
	if got, ok, err := transferred.Ledger.LeaseOf(t.Context(), "workspace"); err != nil || !ok || got.Epoch != lease.Epoch || got.Holder != lease.Holder {
		t.Fatalf("lease was lost: %+v %v %v", got, ok, err)
	}
	if _, err := next.tasks.Create(task.Task{Channel: "session-1", Member: "codex", Goal: "continued remotely"}); err != nil {
		t.Fatal(err)
	}
	if err := stores.state.SetActiveAgent("stale", "bad"); err == nil {
		t.Fatal("old coordinator wrote after transfer")
	}
	if _, err := second.Transfer(t.Context(), coordination.TransferRequest{ID: "return-node-1", Actor: "owner", ExpectedEpoch: 2, TargetNodeID: "node-1", Reason: "user"}); err != nil {
		t.Fatal(err)
	}
	returned := ready(t, first)
	if returned.Generation == initial.Generation || returned.Ledger == initial.Ledger {
		t.Fatal("reactivation reused the old business generation")
	}
	if _, err := stores.tasks.Create(task.Task{Goal: "stale overwrite"}); err == nil {
		t.Fatal("old cached store obtained the new coordinator epoch")
	}
	if got := nodes[0].current().tasks.List("session-1"); len(got) != 2 {
		t.Fatalf("reactivation did not reconstruct cached task data: %+v", got)
	}
	for _, n := range nodes[1:] {
		if err := n.runtime.Swap(nil).Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := returned.Ledger.Document("minority").Save([]byte("unsafe")); err == nil {
		t.Fatal("minority accepted a write")
	}
	if _, ok, _ := first.Ledger().Document("minority").Load(); ok {
		t.Fatal("rejected minority write became visible")
	}
}

func TestAutomaticCoordinatorFailureActivatesReconstructedLedgerOnSurvivor(t *testing.T) {
	nodes := testNodes(t, 3)
	first := openNode(t, nodes[0])
	initial := ready(t, first)
	for _, n := range nodes[1:] {
		joinNode(t, first, n, true, true)
	}
	if err := nodes[0].current().state.SetActiveAgent("running-session", "worker"); err != nil {
		t.Fatal(err)
	}
	created, err := nodes[0].current().tasks.Create(task.Task{Channel: "running-session", Member: "worker", Goal: "survive coordinator failure"})
	if err != nil {
		t.Fatal(err)
	}
	state, err := first.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.SetAutoFailover(t.Context(), coordination.PolicyRequest{ID: "enable", Actor: "owner", ExpectedRevision: state.Revision, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := nodes[0].runtime.Swap(nil).Close(); err != nil {
		t.Fatal(err)
	}
	if initial.Context.Err() == nil {
		t.Fatal("closing coordinator did not revoke its business generation")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer cancel()
	var survivor *Runtime
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for survivor == nil {
		for _, n := range nodes[1:] {
			if n.runtime.Load().Status().Ready {
				survivor = n.runtime.Load()
				break
			}
		}
		if survivor == nil {
			select {
			case <-ctx.Done():
				t.Fatalf("survivors did not activate: node2=%+v node3=%+v", nodes[1].runtime.Load().Status(), nodes[2].runtime.Load().Status())
			case <-ticker.C:
			}
		}
	}
	activation, err := survivor.WaitReady(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := statepkg.OpenLedger(activation.Ledger)
	if err != nil {
		t.Fatal(err)
	}
	if sessions.Conversation("running-session").ActiveAgent != "worker" {
		t.Fatal("automatic transfer lost session identity")
	}
	tasks, err := task.OpenLedger(activation.Ledger)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := tasks.Get(created.ID); !ok || got.Goal != created.Goal {
		t.Fatal("automatic transfer lost durable task")
	}
	if err := sessions.SetActiveAgent("after-failure", "worker"); err != nil {
		t.Fatalf("survivor cannot commit a business write: %v", err)
	}
	current, err := survivor.ReadState(ctx)
	if err != nil || current.Coordinator.Epoch != 2 || !current.AutoFailover {
		t.Fatalf("automatic assignment is not durable: %+v %v", current.Coordinator, err)
	}
	last := current.Audit[len(current.Audit)-1]
	if last.From != "node-1" || last.To != activation.NodeID || last.Reason != "coordinator_unreachable" {
		t.Fatalf("automatic transfer lacks audit provenance: %+v", last)
	}
}

func TestManualNonvoterHubWritesAndFencesOldHub(t *testing.T) {
	nodes := testNodes(t, 2)
	first := openNode(t, nodes[0])
	old := ready(t, first)
	if err := old.Ledger.Document("before-transfer").Save([]byte("preserved")); err != nil {
		t.Fatal(err)
	}
	second := joinNode(t, first, nodes[1], false, false)
	request := coordination.TransferRequest{ID: "to-nonvoter", Actor: "owner", ExpectedEpoch: 1, TargetNodeID: "node-2"}
	if _, err := second.Transfer(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	active := ready(t, second)
	if active.WriterGeneration <= old.WriterGeneration {
		t.Fatal("writer fence did not advance")
	}
	if raw, ok, err := active.Ledger.Document("before-transfer").Load(); err != nil || !ok || string(raw) != "preserved" {
		t.Fatalf("baseline lost: %s %v %v", raw, ok, err)
	}
	if err := active.Ledger.Document("after-transfer").Save([]byte("nonvoter-write")); err != nil {
		t.Fatal(err)
	}
	if err := old.Ledger.Document("old-hub").Save([]byte("unsafe")); err == nil {
		t.Fatal("old hub retained write authority")
	}
	state, err := second.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if state.Coordinator.NodeID != "node-2" || len(state.Voters) != 1 || state.Voters["node-2"] != "" || state.AutoFailover || second.Status().IsLeader {
		t.Fatalf("business transfer changed consensus: %+v", state)
	}
	if _, err := first.service.ApplyApp(t.Context(), coordination.AppCommand{ID: "late-old-hub", CallerNodeID: "node-1", CoordinatorEpoch: 1, WriterGeneration: old.WriterGeneration, ExpectedVersion: state.AppVersion}); !errors.Is(err, coordination.ErrStaleEpoch) {
		t.Fatalf("old epoch crossed durable fence: %v", err)
	}
	if raw, ok, err := first.Ledger().Document("after-transfer").Load(); err != nil || !ok || string(raw) != "nonvoter-write" {
		t.Fatalf("consensus leader lacks nonvoter write: %s %v %v", raw, ok, err)
	}
	// Nonvoter Hub cannot commit alone when its voter quorum disappears.
	if err := nodes[0].runtime.Swap(nil).Close(); err != nil {
		t.Fatal(err)
	}
	if err := active.Ledger.Document("without-quorum").Save([]byte("unsafe")); err == nil {
		t.Fatal("nonvoter hub bypassed quorum")
	}
	if _, ok, _ := second.Ledger().Document("without-quorum").Load(); ok {
		t.Fatal("uncommitted write became visible")
	}
}
