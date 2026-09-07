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

func (n *clusterNode) current() *businessStores {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.business
}

func testNodes(t *testing.T, count int) []*clusterNode {
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
		n.listener, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		n.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if current := n.runtime.Load(); current != nil {
				handler := current.RPCHandler(coordination.RPCOptions{AuthorizeControl: func(*http.Request, coordination.Identity, string) (string, error) { return "test-owner", nil }})
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
		n.client, err = coordination.NewClient(coordination.ClientConfig{TLS: n.options, Members: members, Timeout: time.Second, ControlHeaders: func(context.Context, string) (http.Header, error) {
			return http.Header{"X-Test-Owner": []string{"owner"}}, nil
		}})
		if err != nil {
			t.Fatal(err)
		}
		stream, err := coordination.NewTLSStreamLayer(n.listener, n.options, n.client.PeerID)
		if err != nil {
			t.Fatal(err)
		}
		raftConfig := raft.DefaultConfig()
		raftConfig.HeartbeatTimeout = 180 * time.Millisecond
		raftConfig.ElectionTimeout = 180 * time.Millisecond
		raftConfig.LeaderLeaseTimeout = 90 * time.Millisecond
		raftConfig.CommitTimeout = 10 * time.Millisecond
		n.config = Config{LedgerDir: filepath.Join(t.TempDir(), "ledger"), Client: n.client, PollInterval: 20 * time.Millisecond, ShutdownTimeout: time.Second,
			Coordination: coordination.Config{ClusterID: "test-cluster", NodeID: members[i].NodeID, FailureDomain: "test-domain-" + members[i].NodeID, StorageLevel: "restricted", DataDir: filepath.Join(t.TempDir(), "raft"), Bootstrap: i == 0,
				APIAddress: n.server.URL, StreamLayer: stream, RaftConfig: raftConfig, ApplyTimeout: 3 * time.Second, ProbeInterval: 500 * time.Millisecond, FailoverTimeout: time.Second}}
		n.config.Activate = func(ctx context.Context, activation Activation) (Deactivate, error) {
			sessions, err := statepkg.OpenLedger(activation.Ledger, "")
			if err != nil {
				return nil, err
			}
			tasks, err := task.OpenLedger(activation.Ledger, "")
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
	second := openNode(t, nodes[1])
	member := coordination.Member{NodeID: "node-2", Address: second.Status().Address, APIAddress: nodes[1].server.URL}
	if _, err := first.Join(t.Context(), coordination.JoinRequest{ID: "join-second", Actor: "owner", Member: member}); err != nil {
		t.Fatal(err)
	}
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
	second := openNode(t, nodes[1])
	member := coordination.Member{NodeID: "node-2", Address: second.Status().Address, APIAddress: nodes[1].server.URL}
	if _, err := first.Join(t.Context(), coordination.JoinRequest{ID: "join-second", Actor: "owner", Member: member}); err != nil {
		t.Fatal(err)
	}
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

func TestActivationFailureStopsRuntimeInsteadOfReusingPartialStores(t *testing.T) {
	nodes := testNodes(t, 1)
	failure := errors.New("cannot rebuild business stores")
	nodes[0].config.Activate = func(context.Context, Activation) (Deactivate, error) { return nil, failure }
	r := openNode(t, nodes[0])
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := r.WaitReady(ctx); !errors.Is(err, failure) {
		t.Fatalf("activation failure was not exposed: %v", err)
	}
	if r.Status().Ready || !r.Status().Closed {
		t.Fatal("partially constructed application stayed active")
	}
	// The error was asserted above; suppress the fixture's duplicate report.
	nodes[0].runtime.Store(nil)
	if err := r.Close(); !errors.Is(err, failure) {
		t.Fatalf("closed runtime lost the activation error: %v", err)
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
	second := openNode(t, nodes[1])
	member := coordination.Member{NodeID: "node-2", Address: second.Status().Address, APIAddress: nodes[1].server.URL}
	if _, err := first.Join(t.Context(), coordination.JoinRequest{ID: "join-second", Actor: "owner", Member: member}); err != nil {
		t.Fatal(err)
	}
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
	for i := 1; i < len(nodes); i++ {
		r := openNode(t, nodes[i])
		member := coordination.Member{NodeID: r.Status().NodeID, Address: r.Status().Address, APIAddress: nodes[i].server.URL, AutoEligible: true}
		if _, err := first.Join(t.Context(), coordination.JoinRequest{ID: "join-" + member.NodeID, Actor: "owner", Member: member}); err != nil {
			t.Fatal(err)
		}
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
	for i := 1; i < len(nodes); i++ {
		r := openNode(t, nodes[i])
		member := coordination.Member{NodeID: r.Status().NodeID, Address: r.Status().Address, APIAddress: nodes[i].server.URL, AutoEligible: true}
		if _, err := first.Join(t.Context(), coordination.JoinRequest{ID: "join-" + member.NodeID, Actor: "owner", Member: member}); err != nil {
			t.Fatal(err)
		}
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
	sessions, err := statepkg.OpenLedger(activation.Ledger, "")
	if err != nil {
		t.Fatal(err)
	}
	if sessions.Conversation("running-session").ActiveAgent != "worker" {
		t.Fatal("automatic transfer lost session identity")
	}
	tasks, err := task.OpenLedger(activation.Ledger, "")
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
