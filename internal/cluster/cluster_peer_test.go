package cluster

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/adapter"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/desktop"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/hashicorp/raft"
)

func ClusterPeerTestDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "steve-peer-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func testPeerOptions(t *testing.T, root string, source *Peer) (PeerOptions, *desktop.Installation) {
	t.Helper()
	installed, err := desktop.Bootstrap(desktop.Options{StateDir: root})
	if err != nil {
		t.Fatal(err)
	}
	clusterPath, err := PrepareDesktopCluster(installed.Paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	if source != nil {
		cfg, err := LoadClusterPeerConfig(clusterPath)
		if err != nil {
			t.Fatal(err)
		}
		caPEM, err := ReadClusterPrivate(source.Config.CACertFile)
		if err != nil {
			t.Fatal(err)
		}
		caBlock, _ := pem.Decode(caPEM)
		ca, err := x509.ParseCertificate(caBlock.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		keyPEM, err := ReadClusterPrivate(source.Config.CAKeyFile)
		if err != nil {
			t.Fatal(err)
		}
		keyBlock, _ := pem.Decode(keyPEM)
		key, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		cert, private, err := IssueNodeCertificate(ca, key.(ed25519.PrivateKey), source.Config.ClusterID, cfg.NodeID)
		if err != nil {
			t.Fatal(err)
		}
		cfg.ClusterID = source.Config.ClusterID
		cfg.Bootstrap = false
		cfg.CAKeyFile = ""
		cfg.Seeds = []coordination.Member{{NodeID: source.Config.NodeID, Name: source.Config.Name, Address: source.Config.RaftAddress, APIAddress: source.Config.PeerURL}}
		for _, file := range []struct {
			path string
			data []byte
		}{{cfg.CACertFile, caPEM}, {cfg.CertFile, cert}, {cfg.KeyFile, private}, {cfg.OwnerTokenFile, []byte(source.OwnerToken)}} {
			if err := WritePrivate(file.path, file.data, false); err != nil {
				t.Fatal(err)
			}
		}
		if err := SaveClusterJSON(clusterPath, cfg, false); err != nil {
			t.Fatal(err)
		}
	}
	isolated, err := LoadClusterPeerConfig(clusterPath)
	if err != nil {
		t.Fatal(err)
	}
	isolated.RaftAddress = "127.0.0.1:0"
	isolated.PeerAddress = "127.0.0.1:0"
	isolated.RaftBindAddress = "127.0.0.1:0"
	isolated.PeerBindAddress = "127.0.0.1:0"
	isolated.PeerURL = ""
	if err := SaveClusterJSON(clusterPath, isolated, false); err != nil {
		t.Fatal(err)
	}
	settings := raft.DefaultConfig()
	return PeerOptions{ConfigPath: installed.Paths.Config, ClusterPath: clusterPath, RaftConfig: settings, PollInterval: 25 * time.Millisecond, TestFailureDomain: func() (string, error) { return "test-domain-" + installed.NodeID, nil }}, installed
}

func testPeerApplication(t *testing.T, activations *atomic.Int32) func(context.Context, Activation, func(PeerApplicationEndpoint) error) (Deactivate, error) {
	t.Helper()
	return func(ctx context.Context, activation Activation, ready func(PeerApplicationEndpoint) error) (Deactivate, error) {
		activations.Add(1)
		document := activation.Ledger.Document("peer-test-data")
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		token, err := ClusterRandomToken()
		if err != nil {
			return nil, err
		}
		server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !ConstantToken(r.Header.Get("Authorization"), token) || r.URL.Query().Get("token") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Referer") != "" {
				http.Error(w, "proxy leaked client credentials", http.StatusBadRequest)
				return
			}
			if r.Method == http.MethodPost {
				data, err := io.ReadAll(io.LimitReader(r.Body, 1024))
				if err != nil {
					HTTPError(w, err)
					return
				}
				if err := document.Save(data); err != nil {
					HTTPError(w, err)
					return
				}
			}
			data, _, err := document.Load()
			if err != nil {
				HTTPError(w, err)
				return
			}
			WriteJSON(w, map[string]any{"node_id": activation.NodeID, "generation": activation.Generation, "data": string(data)})
		}), BaseContext: func(net.Listener) context.Context { return ctx }}
		done := make(chan error, 1)
		go func() { done <- server.Serve(listener) }()
		if err := ready(PeerApplicationEndpoint{URL: "http://" + listener.Addr().String(), Token: token}); err != nil {
			server.Close()
			return nil, err
		}
		return func(context.Context) error { server.Close(); <-done; return nil }, nil
	}
}

func StartTestPeer(t *testing.T, options PeerOptions) *Peer {
	t.Helper()
	peer, err := OpenPeer(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := peer.Close(); err != nil {
			t.Errorf("close peer: %v", err)
		}
	})
	return peer
}

func WaitPeerReady(t *testing.T, peer *Peer) Activation {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	active, err := peer.Runtime.Load().WaitReady(ctx)
	if err != nil {
		t.Fatalf("peer did not activate: %v; status=%+v", err, peer.Runtime.Load().Status())
	}
	return active
}

func PeerRequest(t *testing.T, peer *Peer, method, path string, body any) (int, []byte) {
	t.Helper()
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	request, err := http.NewRequest(method, peer.UiURL+path, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+peer.UIToken)
	request.Header.Set("Cookie", "local-secret-cookie")
	request.Header.Set("Referer", peer.UiURL+"/?token="+peer.UIToken)
	client := &http.Client{Timeout: 15 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err = io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, data
}

func TestClusterPeerDesktopStartsWithStableOriginAndPersistentWorker(t *testing.T) {
	options, installed := testPeerOptions(t, ClusterPeerTestDir(t), nil)
	var activations atomic.Int32
	options.Activate = testPeerApplication(t, &activations)
	peer := StartTestPeer(t, options)
	WaitPeerReady(t, peer)
	status, body := PeerRequest(t, peer, http.MethodGet, "/console/test?token="+peer.UIToken, nil)
	if status != http.StatusOK {
		t.Fatalf("application gateway: %d %s", status, body)
	}
	var versions consoleapi.Versions
	status, body = PeerRequest(t, peer, http.MethodGet, "/console/versions", nil)
	if err := json.Unmarshal(body, &versions); err != nil || status != http.StatusOK || versions.HubID != installed.NodeID {
		t.Fatalf("desktop identity endpoint: %d %s %v", status, body, err)
	}
	registry := node.NewRegistry(peer.Config.ClusterID, map[string]node.Config{peer.Worker().Name: {Addr: peer.Worker().Address, Token: peer.Worker().Token, DialContext: peer.DialWorker}})
	if settings, err := registry.Settings(context.Background(), peer.Worker().Name); err != nil || len(settings.Harnesses) != 0 {
		registry.Close()
		t.Fatalf("empty local worker unavailable: %+v %v", settings, err)
	}
	registry.Close()
	before, worker := peer.Config, peer.Worker()
	if err := peer.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := StartTestPeer(t, options)
	WaitPeerReady(t, reopened)
	if reopened.Config.UIAddress != before.UIAddress || reopened.Config.RaftAddress != before.RaftAddress || reopened.Config.PeerURL != before.PeerURL || reopened.Worker() != worker {
		t.Fatal("restart changed a persistent identity or endpoint")
	}
	if activations.Load() != 2 {
		t.Fatalf("business stores were not reconstructed on restart: %d", activations.Load())
	}
	if _, err := desktop.Bootstrap(desktop.Options{StateDir: installed.Paths.Root}); err != nil {
		t.Fatalf("peer altered desktop persistent authentication: %v", err)
	}
}

func TestClusterPeerThreeMembersTransferAndProxyThroughOriginalGateway(t *testing.T) {
	options, _ := testPeerOptions(t, ClusterPeerTestDir(t), nil)
	var firstActivations, secondActivations, thirdActivations atomic.Int32
	options.Activate = testPeerApplication(t, &firstActivations)
	first := StartTestPeer(t, options)
	WaitPeerReady(t, first)
	secondOptions, _ := testPeerOptions(t, ClusterPeerTestDir(t), first)
	secondOptions.Activate = testPeerApplication(t, &secondActivations)
	second := StartTestPeer(t, secondOptions)
	thirdOptions, _ := testPeerOptions(t, ClusterPeerTestDir(t), first)
	thirdOptions.Activate = testPeerApplication(t, &thirdActivations)
	third := StartTestPeer(t, thirdOptions)
	for _, peer := range []*Peer{second, third} {
		_, err := first.Join(context.Background(), coordination.JoinRequest{ID: "join-" + peer.Config.NodeID, Actor: "owner", Member: coordination.Member{NodeID: peer.Config.NodeID, Name: peer.Config.Name, Address: peer.Config.RaftAddress, APIAddress: peer.Config.PeerURL}})
		if err != nil {
			t.Fatal(err)
		}
	}
	descriptor, err := first.FetchWorker(context.Background(), coordination.Member{NodeID: second.Config.NodeID, APIAddress: second.Config.PeerURL})
	if err != nil || descriptor != second.Worker() {
		t.Fatalf("private worker descriptor did not match authenticated node: %v", err)
	}
	status, body := PeerRequest(t, first, http.MethodGet, clusterWorkerPath+"/descriptor", nil)
	if status != http.StatusNotFound || bytes.Contains(body, []byte(second.Worker().Token)) {
		t.Fatal("worker descriptor leaked onto the browser gateway")
	}
	status, body = PeerRequest(t, first, http.MethodPost, "/console/test", map[string]string{"message": "preserved"})
	if status != http.StatusOK {
		t.Fatalf("write before transfer: %d %s", status, body)
	}
	status, body = PeerRequest(t, first, http.MethodPost, "/console/coordination/transfer", consoleapi.CoordinatorTransfer{CommandID: "to-second", ExpectedEpoch: 1, TargetNodeID: second.Config.NodeID})
	if status != http.StatusOK {
		t.Fatalf("transfer: %d %s", status, body)
	}
	WaitPeerReady(t, second)
	status, body = PeerRequest(t, first, http.MethodGet, "/console/test?token="+first.UIToken, nil)
	var result struct {
		NodeID string `json:"node_id"`
		Data   string `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil || status != http.StatusOK || result.NodeID != second.Config.NodeID || result.Data != "{\"message\":\"preserved\"}" {
		t.Fatalf("stable original gateway did not reach new coordinator with preserved data: %d %s %v", status, body, err)
	}
	worker := first.Worker()
	registry := node.NewRegistry(first.Config.ClusterID, map[string]node.Config{worker.Name: {Addr: "127.0.0.1:1", Token: worker.Token, DialContext: second.DialWorker}})
	defer registry.Close()
	if _, err := registry.Settings(context.Background(), worker.Name); err != nil {
		registry.Close()
		t.Fatalf("new coordinator cannot reach original machine's worker via TLS tunnel: %v", err)
	}
	if first.Worker() != worker {
		t.Fatal("coordinator handoff restarted the physical worker")
	}
	status, body = PeerRequest(t, first, http.MethodPost, "/console/coordination/transfer", consoleapi.CoordinatorTransfer{CommandID: "back-to-first", ExpectedEpoch: 2, TargetNodeID: first.Config.NodeID})
	if status != http.StatusOK {
		t.Fatalf("return transfer: %d %s", status, body)
	}
	WaitPeerReady(t, first)
	deadline := time.Now().Add(3 * time.Second)
	revoked := false
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := registry.Settings(ctx, worker.Name)
		cancel()
		if err != nil {
			revoked = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !revoked {
		t.Fatal("previous coordinator retained an authenticated worker control connection")
	}
	if firstActivations.Load() != 2 || secondActivations.Load() != 1 || thirdActivations.Load() != 0 {
		t.Fatalf("incorrect application generations: %d/%d/%d", firstActivations.Load(), secondActivations.Load(), thirdActivations.Load())
	}
	state, err := first.Runtime.Load().ReadState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	status, body = PeerRequest(t, first, http.MethodPut, "/console/coordination/policy", consoleapi.CoordinatorPolicy{CommandID: "not-yet-ready", ExpectedRevision: state.Revision, Enabled: true})
	if status == http.StatusOK || first.Runtime.Load().Status().AutoFailover {
		t.Fatalf("automatic task recovery was enabled before consumer integration: %d %s", status, body)
	}
}

func TestClusterPeerRestartCanVoteWithMemberAddedAfterItsSnapshot(t *testing.T) {
	options, _ := testPeerOptions(t, ClusterPeerTestDir(t), nil)
	var starts atomic.Int32
	options.Activate = testPeerApplication(t, &starts)
	first := StartTestPeer(t, options)
	WaitPeerReady(t, first)
	secondOptions, _ := testPeerOptions(t, ClusterPeerTestDir(t), first)
	secondOptions.Activate = testPeerApplication(t, &starts)
	second := StartTestPeer(t, secondOptions)
	_, err := first.Join(context.Background(), coordination.JoinRequest{ID: "join-second", Actor: "owner", Member: coordination.Member{NodeID: second.Config.NodeID, Address: second.Config.RaftAddress, APIAddress: second.Config.PeerURL}})
	if err != nil {
		t.Fatal(err)
	}
	thirdOptions, _ := testPeerOptions(t, ClusterPeerTestDir(t), first)
	thirdOptions.Activate = testPeerApplication(t, &starts)
	third := StartTestPeer(t, thirdOptions)
	_, err = first.Join(context.Background(), coordination.JoinRequest{ID: "join-third", Actor: "owner", Member: coordination.Member{NodeID: third.Config.NodeID, Address: third.Config.RaftAddress, APIAddress: third.Config.PeerURL}})
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := StartTestPeer(t, secondOptions)
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		state, err := third.Runtime.Load().ReadState(ctx)
		cancel()
		if err == nil && len(state.Voters) == 3 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("surviving peers could not recover quorum after restart: %s/%s", restarted.Runtime.Load().Status().LeaderID, third.Runtime.Load().Status().LeaderID)
}

func TestClusterPeerInitializationRecoversPublishedAuthorityWithoutSidecar(t *testing.T) {
	options, installed := testPeerOptions(t, ClusterPeerTestDir(t), nil)
	before, err := LoadClusterPeerConfig(options.ClusterPath)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := ReadClusterPrivate(before.CertFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(options.ClusterPath); err != nil {
		t.Fatal(err)
	}
	path, err := PrepareDesktopCluster(installed.Paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	after, err := LoadClusterPeerConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := ReadClusterPrivate(after.CertFile)
	if err != nil {
		t.Fatal(err)
	}
	if before.ClusterID != after.ClusterID || !bytes.Equal(certificate, recovered) {
		t.Fatal("initialization recovery replaced published identity")
	}
}

func TestClusterPeerRestoresRegisteredAdapterFromVerifiedLocalInstallation(t *testing.T) {
	dir := t.TempDir()
	name := "codex-acp"
	pin := adapter.Catalog[name]
	installed := filepath.Join(dir, "adapters", name+"@"+pin.Version)
	command := filepath.Join(installed, pin.Bin)
	if err := os.MkdirAll(filepath.Dir(command), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(command, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	marker, _ := json.Marshal(map[string]string{"package": pin.Package, "version": pin.Version, "integrity": pin.Integrity})
	if err := os.WriteFile(filepath.Join(installed, ".steve-adapter.json"), marker, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := node.ServerConfig{StateDir: dir, Harnesses: map[string]node.HarnessSpec{"codex": {Adapter: name}, "missing": {Adapter: "claude-agent-acp", Command: "obsolete-command"}, "custom": {Command: "custom-command"}}}
	restorePeerAdapters(&cfg)
	if cfg.Harnesses["codex"].Command != command || cfg.Harnesses["missing"].Command != "" || cfg.Harnesses["custom"].Command != "custom-command" {
		t.Fatalf("registered adapter restoration failed: %+v", cfg.Harnesses)
	}
}
