package coordination

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/raft"
)

type testAuthority struct {
	key         ed25519.PrivateKey
	certificate *x509.Certificate
	roots       *x509.CertPool
}

func newTestAuthority(t *testing.T) *testAuthority {
	t.Helper()
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "isolated-test-cluster"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	return &testAuthority{key: key, certificate: certificate, roots: roots}
}

func (a *testAuthority) node(t *testing.T, clusterID, nodeID string) TLSOptions {
	t.Helper()
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: nodeID}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}, URIs: []*url.URL{IdentityURI(clusterID, nodeID)}}
	der, err := x509.CreateCertificate(rand.Reader, template, a.certificate, public, a.key)
	if err != nil {
		t.Fatal(err)
	}
	return TLSOptions{ClusterID: clusterID, NodeID: nodeID, Certificate: tls.Certificate{Certificate: [][]byte{der, a.certificate.Raw}, PrivateKey: key}, RootCAs: a.roots, HandshakeTimeout: time.Second}
}

type tlsTestCluster struct {
	*testCluster
	clients    map[string]*Client
	servers    map[string]*http.Server
	identities map[string]TLSOptions
	members    map[string]Member
	authority  *testAuthority
}

func newTLSTestCluster(t *testing.T, count int) *tlsTestCluster {
	t.Helper()
	c := &tlsTestCluster{testCluster: &testCluster{t: t, nodes: map[string]*Service{}, configs: map[string]Config{}}, clients: map[string]*Client{}, servers: map[string]*http.Server{}, identities: map[string]TLSOptions{}, members: map[string]Member{}, authority: newTestAuthority(t)}
	raftListeners := map[string]net.Listener{}
	apiListeners := map[string]net.Listener{}
	var seeds []Member
	for i := 1; i <= count; i++ {
		id := fmt.Sprintf("node-%d", i)
		raftListener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		apiListener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		raftListeners[id] = raftListener
		apiListeners[id] = apiListener
		member := Member{NodeID: id, Address: raftListener.Addr().String(), APIAddress: "https://" + apiListener.Addr().String()}
		c.members[id] = member
		seeds = append(seeds, member)
	}
	for i := 1; i <= count; i++ {
		id := fmt.Sprintf("node-%d", i)
		identity := c.authority.node(t, "tls-test", id)
		c.identities[id] = identity
		client, err := NewClient(ClientConfig{TLS: identity, Members: seeds, Timeout: time.Second, ControlHeaders: func(context.Context, string) (http.Header, error) {
			return http.Header{"Authorization": []string{"Bearer test-owner"}}, nil
		}})
		if err != nil {
			t.Fatal(err)
		}
		c.clients[id] = client
		var local atomic.Pointer[Service]
		identity.AuthorizePeer = func(peer Identity) bool {
			if peer.ClusterID != identity.ClusterID {
				return false
			}
			if service := local.Load(); service != nil {
				state := service.Status()
				if len(state.Members) > 0 {
					_, ok := state.Members[peer.NodeID]
					return ok
				}
			}
			_, ok := c.members[peer.NodeID]
			return ok
		}
		c.identities[id] = identity
		stream, err := NewTLSStreamLayer(raftListeners[id], identity, func(address raft.ServerAddress) string {
			if service := local.Load(); service != nil {
				for nodeID, member := range service.Status().Members {
					if member.Address == string(address) {
						return nodeID
					}
				}
			}
			return client.PeerID(address)
		})
		if err != nil {
			t.Fatal(err)
		}
		cfg := raft.DefaultConfig()
		cfg.HeartbeatTimeout = 200 * time.Millisecond
		cfg.ElectionTimeout = 200 * time.Millisecond
		cfg.LeaderLeaseTimeout = 100 * time.Millisecond
		cfg.CommitTimeout = 10 * time.Millisecond
		dir := t.TempDir()
		app := openCounter(t, dir)
		if i == 1 {
			app.count = 17
			if err := app.persist(); err != nil {
				t.Fatal(err)
			}
		}
		config := Config{ClusterID: "tls-test", NodeID: id, FailureDomain: "test-domain-" + id, StorageLevel: "restricted", DataDir: dir, Bootstrap: i == 1, APIAddress: c.members[id].APIAddress, Application: app, StreamLayer: stream, Probe: client.Probe, RaftConfig: cfg, ApplyTimeout: 2 * time.Second, ProbeInterval: 250 * time.Millisecond, FailoverTimeout: time.Second, LogOutput: io.Discard}
		node, err := Open(config)
		if err != nil {
			t.Fatal(err)
		}
		local.Store(node)
		c.mu.Lock()
		c.nodes[id] = node
		c.mu.Unlock()
		c.configs[id] = config
		serverTLS, err := identity.ServerConfig()
		if err != nil {
			t.Fatal(err)
		}
		server := &http.Server{Handler: NewRPCHandler(node, RPCOptions{AuthorizeControl: func(r *http.Request, _ Identity, _ string) (string, error) {
			if r.Header.Get("Authorization") != "Bearer test-owner" {
				return "", errors.New("owner token missing")
			}
			return "owner:test-user", nil
		}}), ReadHeaderTimeout: time.Second, TLSConfig: serverTLS}
		c.servers[id] = server
		go server.Serve(tls.NewListener(apiListeners[id], serverTLS))
	}
	t.Cleanup(func() {
		for _, server := range c.servers {
			server.Close()
		}
		for _, client := range c.clients {
			client.Close()
		}
		for _, node := range c.nodes {
			node.Close()
		}
	})
	c.leader()
	for i := 2; i <= count; i++ {
		id := fmt.Sprintf("node-%d", i)
		_, err := c.clients["node-1"].Join(context.Background(), JoinRequest{ID: "join-" + id, Actor: "untrusted-body", Member: c.members[id]})
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.clients["node-1"].BeginWriter(t.Context(), WriterRequest{ID: "initial-writer", CallerNodeID: "node-1", CoordinatorEpoch: 1}); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestMutualTLSRPCJoinsReplicatesAndRoutesPastFollower(t *testing.T) {
	c := newTLSTestCluster(t, 3)
	if value := c.configs["node-2"].Application.(*durableCounter).value(); value != 17 {
		t.Fatalf("TLS join missed snapshot baseline: %d", value)
	}
	client := c.clients["node-1"]
	// Force the first request through a follower and use its authenticated leader
	// hint. The client retains the same command ID while rerouting.
	client.mu.Lock()
	client.leader = "node-3"
	client.mu.Unlock()
	result, err := client.ApplyApp(context.Background(), AppCommand{WriterGeneration: 1, ID: "rpc-write", CallerNodeID: "node-1", CoordinatorEpoch: 1, Payload: []byte("4")})
	if err != nil || string(result.Data) != "21" {
		t.Fatalf("routed write failed: %+v %v", result, err)
	}
	request := TransferRequest{ID: "rpc-transfer", Actor: "spoofed-owner", ExpectedEpoch: 1, TargetNodeID: "node-2", Reason: "manual"}
	transfer, err := client.Transfer(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if transfer.Coordinator != (Assignment{NodeID: "node-2", Epoch: 2}) {
		t.Fatalf("transfer failed: %+v", transfer)
	}
	state, err := client.ReadState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if actor := state.Audit[len(state.Audit)-1].Actor; actor != "owner:test-user" {
		t.Fatalf("RPC trusted supplied actor: %s", actor)
	}
	result, err = c.clients["node-2"].ApplyApp(context.Background(), AppCommand{WriterGeneration: 1, ID: "rpc-new-coordinator", CallerNodeID: "node-2", CoordinatorEpoch: 2, ExpectedVersion: 1, Payload: []byte("2")})
	if err != nil || string(result.Data) != "23" {
		t.Fatalf("business coordinator could not write through different leader: %+v %v", result, err)
	}
}

func TestRPCRejectsSpoofedCallerAndMissingOwnerAuthorization(t *testing.T) {
	c := newTLSTestCluster(t, 2)
	client, err := NewClient(ClientConfig{TLS: c.identities["node-2"], Members: []Member{c.members["node-1"]}, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_, err = client.request(context.Background(), c.members["node-1"], "app", []byte(`{"id":"spoof","caller_node_id":"node-1","coordinator_epoch":1,"expected_version":0,"payload":"MQ=="}`), nil, &Result{})
	if !errors.Is(err, ErrNotCoordinator) {
		t.Fatalf("body spoofed TLS caller identity: %v", err)
	}
	_, err = client.request(context.Background(), c.members["node-1"], "transfer", []byte(`{"id":"unauthorized","actor":"owner","expected_epoch":1,"target_node_id":"node-2","reason":"manual"}`), nil, &Result{})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("management accepted without owner authorization: %v", err)
	}
	recorder := httptest.NewRecorder()
	NewRPCHandler(c.nodes["node-1"], RPCOptions{}).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, RPCPath+"status", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("plain HTTP accepted: %d", recorder.Code)
	}
}

func TestPeerTLSRejectsWrongClusterCAAndNodeIdentity(t *testing.T) {
	c := newTLSTestCluster(t, 1)
	cases := []struct {
		name     string
		identity TLSOptions
		member   Member
	}{
		{name: "wrong-node", identity: c.authority.node(t, "tls-test", "client"), member: Member{NodeID: "different-node", APIAddress: c.members["node-1"].APIAddress}},
		{name: "wrong-cluster", identity: c.authority.node(t, "another-cluster", "client"), member: c.members["node-1"]},
		{name: "wrong-ca", identity: newTestAuthority(t).node(t, "tls-test", "client"), member: c.members["node-1"]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, err := NewClient(ClientConfig{TLS: tc.identity, Timeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			if _, err := client.Status(context.Background(), tc.member); err == nil {
				t.Fatal("untrusted peer accepted")
			}
		})
	}
}

func TestRemoveConsensusLeaderAfterCoordinatorTransferRoutesAndCommits(t *testing.T) {
	c := newTLSTestCluster(t, 3)
	client := c.clients["node-2"]
	_, err := client.Transfer(context.Background(), TransferRequest{ID: "move-before-remove", Actor: "owner", ExpectedEpoch: 1, TargetNodeID: "node-2"})
	if err != nil {
		t.Fatal(err)
	}
	request := RemoveRequest{ID: "remove-old-node", Actor: "owner", NodeID: "node-1"}
	_, err = client.Remove(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	state, err := client.ReadState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Voters) != 2 || len(state.Members) != 2 || len(state.Removing) != 0 || state.Coordinator.NodeID != "node-2" {
		t.Fatalf("incomplete removal: %+v", state)
	}
	if _, err := client.Remove(context.Background(), request); err != nil {
		t.Fatalf("removal retry was not idempotent: %v", err)
	}
}

func TestUnjoinedClusterCertificateCannotSendRaftRPC(t *testing.T) {
	c := newTLSTestCluster(t, 1)
	identity := c.authority.node(t, "tls-test", "unjoined-node")
	identity.AuthorizePeer = func(Identity) bool { return true }
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stream, err := NewTLSStreamLayer(listener, identity, func(raft.ServerAddress) string { return "node-1" })
	if err != nil {
		t.Fatal(err)
	}
	transport := raft.NewNetworkTransport(stream, 1, time.Second, io.Discard)
	defer transport.Close()
	defer transport.CloseStreams()
	request := &raft.AppendEntriesRequest{RPCHeader: raft.RPCHeader{ProtocolVersion: raft.ProtocolVersionMax, ID: []byte("forged-leader"), Addr: []byte("127.0.0.1:49999")}, Term: 1000000}
	var response raft.AppendEntriesResponse
	if err := transport.AppendEntries("node-1", raft.ServerAddress(c.members["node-1"].Address), request, &response); err == nil {
		t.Fatalf("unjoined certificate reached Raft: %+v", response)
	}
	if c.nodes["node-1"].Status().LeaderID != "node-1" {
		t.Fatal("unjoined peer affected consensus leadership")
	}
}

func TestRevokingRaftPeerClosesExistingAuthenticatedConnections(t *testing.T) {
	authority := newTestAuthority(t)
	serverIdentity := authority.node(t, "revocation-test", "server")
	var allowed atomic.Bool
	allowed.Store(true)
	serverIdentity.AuthorizePeer = func(peer Identity) bool { return peer.NodeID == "client" && allowed.Load() }
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stream, err := NewTLSStreamLayer(listener, serverIdentity, func(raft.ServerAddress) string { return "client" })
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		connection, err := stream.Accept()
		if err == nil {
			var first [1]byte
			connection.Read(first[:])
			accepted <- connection
		}
	}()
	clientIdentity := authority.node(t, "revocation-test", "client")
	clientTLS, err := clientIdentity.ClientConfig("server")
	if err != nil {
		t.Fatal(err)
	}
	client, err := tls.Dial("tcp", listener.Addr().String(), clientTLS)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	select {
	case connection := <-accepted:
		defer connection.Close()
	case <-time.After(time.Second):
		t.Fatal("peer did not authenticate")
	}
	allowed.Store(false)
	stream.RevokeUnauthorized()
	client.SetReadDeadline(time.Now().Add(time.Second))
	var b [1]byte
	if _, err := client.Read(b[:]); err == nil {
		t.Fatal("revoked peer retained authenticated connection")
	}
}
