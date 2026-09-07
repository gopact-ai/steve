package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/adapter"
	"github.com/gopact-ai/steve/internal/cluster"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/desktop"
	"github.com/gopact-ai/steve/internal/httpapi"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/hashicorp/raft"
)

func clusterPeerTestDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "steve-peer-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func testPeerOptions(t *testing.T, root string, source *clusterPeer) (clusterPeerOptions, *desktop.Installation) {
	t.Helper()
	installed, err := desktop.Bootstrap(desktop.Options{StateDir: root})
	if err != nil {
		t.Fatal(err)
	}
	clusterPath, err := prepareDesktopCluster(installed.Paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	if source != nil {
		cfg, err := loadClusterPeerConfig(clusterPath)
		if err != nil {
			t.Fatal(err)
		}
		caPEM, err := readClusterPrivate(source.config.CACertFile)
		if err != nil {
			t.Fatal(err)
		}
		caBlock, _ := pem.Decode(caPEM)
		ca, err := x509.ParseCertificate(caBlock.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		keyPEM, err := readClusterPrivate(source.config.CAKeyFile)
		if err != nil {
			t.Fatal(err)
		}
		keyBlock, _ := pem.Decode(keyPEM)
		key, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		cert, private, err := issueClusterNodeCertificate(ca, key.(ed25519.PrivateKey), source.config.ClusterID, cfg.NodeID)
		if err != nil {
			t.Fatal(err)
		}
		cfg.ClusterID = source.config.ClusterID
		cfg.Bootstrap = false
		cfg.CAKeyFile = ""
		cfg.Seeds = []coordination.Member{{NodeID: source.config.NodeID, Name: source.config.Name, Address: source.config.RaftAddress, APIAddress: source.config.PeerURL}}
		for _, file := range []struct {
			path string
			data []byte
		}{{cfg.CACertFile, caPEM}, {cfg.CertFile, cert}, {cfg.KeyFile, private}, {cfg.OwnerTokenFile, []byte(source.ownerToken)}} {
			if err := writeClusterPrivate(file.path, file.data, false); err != nil {
				t.Fatal(err)
			}
		}
		if err := saveClusterJSON(clusterPath, cfg, false); err != nil {
			t.Fatal(err)
		}
	}
	isolated, err := loadClusterPeerConfig(clusterPath)
	if err != nil {
		t.Fatal(err)
	}
	isolated.RaftAddress = "127.0.0.1:0"
	isolated.PeerAddress = "127.0.0.1:0"
	isolated.RaftBindAddress = "127.0.0.1:0"
	isolated.PeerBindAddress = "127.0.0.1:0"
	isolated.PeerURL = ""
	if err := saveClusterJSON(clusterPath, isolated, false); err != nil {
		t.Fatal(err)
	}
	settings := raft.DefaultConfig()
	return clusterPeerOptions{ConfigPath: installed.Paths.Config, ClusterPath: clusterPath, RaftConfig: settings, PollInterval: 25 * time.Millisecond, TestFailureDomain: func() (string, error) { return "test-domain-" + installed.NodeID, nil }}, installed
}

func testPeerApplication(t *testing.T, activations *atomic.Int32) func(context.Context, cluster.Activation, func(peerApplicationEndpoint) error) (cluster.Deactivate, error) {
	t.Helper()
	return func(ctx context.Context, activation cluster.Activation, ready func(peerApplicationEndpoint) error) (cluster.Deactivate, error) {
		activations.Add(1)
		document := activation.Ledger.Document("peer-test-data")
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		token, err := clusterRandomToken()
		if err != nil {
			return nil, err
		}
		server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !constantToken(r.Header.Get("Authorization"), token) || r.URL.Query().Get("token") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Referer") != "" {
				http.Error(w, "proxy leaked client credentials", http.StatusBadRequest)
				return
			}
			if r.Method == http.MethodPost {
				data, err := io.ReadAll(io.LimitReader(r.Body, 1024))
				if err != nil {
					peerHTTPError(w, err)
					return
				}
				if err := document.Save(data); err != nil {
					peerHTTPError(w, err)
					return
				}
			}
			data, _, err := document.Load()
			if err != nil {
				peerHTTPError(w, err)
				return
			}
			writePeerJSON(w, map[string]any{"node_id": activation.NodeID, "generation": activation.Generation, "data": string(data)})
		}), BaseContext: func(net.Listener) context.Context { return ctx }}
		done := make(chan error, 1)
		go func() { done <- server.Serve(listener) }()
		if err := ready(peerApplicationEndpoint{URL: "http://" + listener.Addr().String(), Token: token}); err != nil {
			server.Close()
			return nil, err
		}
		return func(context.Context) error { server.Close(); <-done; return nil }, nil
	}
}

func startTestPeer(t *testing.T, options clusterPeerOptions) *clusterPeer {
	t.Helper()
	peer, err := openClusterPeer(context.Background(), options)
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

func waitPeerReady(t *testing.T, peer *clusterPeer) cluster.Activation {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	active, err := peer.runtime.Load().WaitReady(ctx)
	if err != nil {
		t.Fatalf("peer did not activate: %v; status=%+v", err, peer.runtime.Load().Status())
	}
	return active
}

func peerRequest(t *testing.T, peer *clusterPeer, method, path string, body any) (int, []byte) {
	t.Helper()
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	request, err := http.NewRequest(method, peer.uiURL+path, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+peer.uiToken)
	request.Header.Set("Cookie", "local-secret-cookie")
	request.Header.Set("Referer", peer.uiURL+"/?token="+peer.uiToken)
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
	options, installed := testPeerOptions(t, clusterPeerTestDir(t), nil)
	var activations atomic.Int32
	options.Activate = testPeerApplication(t, &activations)
	peer := startTestPeer(t, options)
	waitPeerReady(t, peer)
	status, body := peerRequest(t, peer, http.MethodGet, "/console/test?token="+peer.uiToken, nil)
	if status != http.StatusOK {
		t.Fatalf("application gateway: %d %s", status, body)
	}
	var versions consoleapi.Versions
	status, body = peerRequest(t, peer, http.MethodGet, "/console/versions", nil)
	if err := json.Unmarshal(body, &versions); err != nil || status != http.StatusOK || versions.HubID != installed.NodeID {
		t.Fatalf("desktop identity endpoint: %d %s %v", status, body, err)
	}
	registry := node.NewRegistry(peer.config.ClusterID, map[string]node.Config{peer.Worker().Name: {Addr: peer.Worker().Address, Token: peer.Worker().Token, DialContext: peer.DialWorker}})
	if settings, err := registry.Settings(context.Background(), peer.Worker().Name); err != nil || len(settings.Harnesses) != 0 {
		registry.Close()
		t.Fatalf("empty local worker unavailable: %+v %v", settings, err)
	}
	registry.Close()
	before, worker := peer.config, peer.Worker()
	if err := peer.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := startTestPeer(t, options)
	waitPeerReady(t, reopened)
	if reopened.config.UIAddress != before.UIAddress || reopened.config.RaftAddress != before.RaftAddress || reopened.config.PeerURL != before.PeerURL || reopened.Worker() != worker {
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
	options, _ := testPeerOptions(t, clusterPeerTestDir(t), nil)
	var firstActivations, secondActivations, thirdActivations atomic.Int32
	options.Activate = testPeerApplication(t, &firstActivations)
	first := startTestPeer(t, options)
	waitPeerReady(t, first)
	secondOptions, _ := testPeerOptions(t, clusterPeerTestDir(t), first)
	secondOptions.Activate = testPeerApplication(t, &secondActivations)
	second := startTestPeer(t, secondOptions)
	thirdOptions, _ := testPeerOptions(t, clusterPeerTestDir(t), first)
	thirdOptions.Activate = testPeerApplication(t, &thirdActivations)
	third := startTestPeer(t, thirdOptions)
	for _, peer := range []*clusterPeer{second, third} {
		_, err := first.Join(context.Background(), coordination.JoinRequest{ID: "join-" + peer.config.NodeID, Actor: "owner", Member: coordination.Member{NodeID: peer.config.NodeID, Name: peer.config.Name, Address: peer.config.RaftAddress, APIAddress: peer.config.PeerURL}})
		if err != nil {
			t.Fatal(err)
		}
	}
	descriptor, err := first.FetchWorker(context.Background(), coordination.Member{NodeID: second.config.NodeID, APIAddress: second.config.PeerURL})
	if err != nil || descriptor != second.Worker() {
		t.Fatalf("private worker descriptor did not match authenticated node: %v", err)
	}
	status, body := peerRequest(t, first, http.MethodGet, clusterWorkerPath+"/descriptor", nil)
	if status != http.StatusNotFound || bytes.Contains(body, []byte(second.Worker().Token)) {
		t.Fatal("worker descriptor leaked onto the browser gateway")
	}
	status, body = peerRequest(t, first, http.MethodPost, "/console/test", map[string]string{"message": "preserved"})
	if status != http.StatusOK {
		t.Fatalf("write before transfer: %d %s", status, body)
	}
	status, body = peerRequest(t, first, http.MethodPost, "/console/coordination/transfer", consoleapi.CoordinatorTransfer{CommandID: "to-second", ExpectedEpoch: 1, TargetNodeID: second.config.NodeID})
	if status != http.StatusOK {
		t.Fatalf("transfer: %d %s", status, body)
	}
	waitPeerReady(t, second)
	status, body = peerRequest(t, first, http.MethodGet, "/console/test?token="+first.uiToken, nil)
	var result struct {
		NodeID string `json:"node_id"`
		Data   string `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil || status != http.StatusOK || result.NodeID != second.config.NodeID || result.Data != "{\"message\":\"preserved\"}" {
		t.Fatalf("stable original gateway did not reach new coordinator with preserved data: %d %s %v", status, body, err)
	}
	worker := first.Worker()
	registry := node.NewRegistry(first.config.ClusterID, map[string]node.Config{worker.Name: {Addr: "127.0.0.1:1", Token: worker.Token, DialContext: second.DialWorker}})
	defer registry.Close()
	if _, err := registry.Settings(context.Background(), worker.Name); err != nil {
		registry.Close()
		t.Fatalf("new coordinator cannot reach original machine's worker via TLS tunnel: %v", err)
	}
	if first.Worker() != worker {
		t.Fatal("coordinator handoff restarted the physical worker")
	}
	status, body = peerRequest(t, first, http.MethodPost, "/console/coordination/transfer", consoleapi.CoordinatorTransfer{CommandID: "back-to-first", ExpectedEpoch: 2, TargetNodeID: first.config.NodeID})
	if status != http.StatusOK {
		t.Fatalf("return transfer: %d %s", status, body)
	}
	waitPeerReady(t, first)
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
	state, err := first.runtime.Load().ReadState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	status, body = peerRequest(t, first, http.MethodPut, "/console/coordination/policy", consoleapi.CoordinatorPolicy{CommandID: "not-yet-ready", ExpectedRevision: state.Revision, Enabled: true})
	if status == http.StatusOK || first.runtime.Load().Status().AutoFailover {
		t.Fatalf("automatic task recovery was enabled before consumer integration: %d %s", status, body)
	}
}

func TestClusterPeerDoesNotOptOrdinaryCLIIntoClusterMode(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	handled, err := maybeManagedPeer([]string{"--config", configPath})
	if handled || err != nil {
		t.Fatalf("ordinary CLI unexpectedly entered managed mode: %v %v", handled, err)
	}
	if _, err := os.Stat(defaultClusterConfigPath(configPath)); !os.IsNotExist(err) {
		t.Fatalf("ordinary CLI created cluster configuration: %v", err)
	}
}

func TestClusterPeerActualApplicationActivates(t *testing.T) {
	options, _ := testPeerOptions(t, clusterPeerTestDir(t), nil)
	peer := startTestPeer(t, options)
	waitPeerReady(t, peer)
	status, body := peerRequest(t, peer, http.MethodGet, "/state", nil)
	if status != http.StatusOK {
		t.Fatalf("actual application failed: %d %s", status, body)
	}
	if err := peer.Close(); err != nil {
		t.Fatal(fmt.Errorf("shutdown actual application: %w", err))
	}
}

func TestClusterPeerActualApplicationsRebuildAcrossThreePeerTransfer(t *testing.T) {
	options, _ := testPeerOptions(t, clusterPeerTestDir(t), nil)
	var firstStarts, secondStarts atomic.Int32
	options.ApplicationReady = func(*fleetAdmin, *httpapi.Server, cluster.Activation) error { firstStarts.Add(1); return nil }
	first := startTestPeer(t, options)
	waitPeerReady(t, first)
	secondOptions, _ := testPeerOptions(t, clusterPeerTestDir(t), first)
	secondOptions.ApplicationReady = func(*fleetAdmin, *httpapi.Server, cluster.Activation) error { secondStarts.Add(1); return nil }
	second := startTestPeer(t, secondOptions)
	thirdOptions, _ := testPeerOptions(t, clusterPeerTestDir(t), first)
	third := startTestPeer(t, thirdOptions)
	for _, peer := range []*clusterPeer{second, third} {
		_, err := first.Join(context.Background(), coordination.JoinRequest{ID: "real-join-" + peer.config.NodeID, Actor: "owner", Member: coordination.Member{NodeID: peer.config.NodeID, Address: peer.config.RaftAddress, APIAddress: peer.config.PeerURL}})
		if err != nil {
			t.Fatal(err)
		}
	}
	status, body := peerRequest(t, first, http.MethodPost, "/console/coordination/transfer", consoleapi.CoordinatorTransfer{CommandID: "real-transfer", ExpectedEpoch: 1, TargetNodeID: second.config.NodeID})
	if status != http.StatusOK {
		t.Fatalf("real application transfer: %d %s", status, body)
	}
	waitPeerReady(t, second)
	status, body = peerRequest(t, first, http.MethodGet, "/state", nil)
	if status != http.StatusOK {
		t.Fatalf("original UI could not access second real application: %d %s", status, body)
	}
	status, body = peerRequest(t, first, http.MethodPost, "/console/coordination/transfer", consoleapi.CoordinatorTransfer{CommandID: "real-return", ExpectedEpoch: 2, TargetNodeID: first.config.NodeID})
	if status != http.StatusOK {
		t.Fatalf("real application return: %d %s", status, body)
	}
	waitPeerReady(t, first)
	if firstStarts.Load() != 2 || secondStarts.Load() != 1 {
		t.Fatalf("real application stores not reconstructed: %d/%d", firstStarts.Load(), secondStarts.Load())
	}
}

func TestClusterPeerRestartCanVoteWithMemberAddedAfterItsSnapshot(t *testing.T) {
	options, _ := testPeerOptions(t, clusterPeerTestDir(t), nil)
	var starts atomic.Int32
	options.Activate = testPeerApplication(t, &starts)
	first := startTestPeer(t, options)
	waitPeerReady(t, first)
	secondOptions, _ := testPeerOptions(t, clusterPeerTestDir(t), first)
	secondOptions.Activate = testPeerApplication(t, &starts)
	second := startTestPeer(t, secondOptions)
	_, err := first.Join(context.Background(), coordination.JoinRequest{ID: "join-second", Actor: "owner", Member: coordination.Member{NodeID: second.config.NodeID, Address: second.config.RaftAddress, APIAddress: second.config.PeerURL}})
	if err != nil {
		t.Fatal(err)
	}
	thirdOptions, _ := testPeerOptions(t, clusterPeerTestDir(t), first)
	thirdOptions.Activate = testPeerApplication(t, &starts)
	third := startTestPeer(t, thirdOptions)
	_, err = first.Join(context.Background(), coordination.JoinRequest{ID: "join-third", Actor: "owner", Member: coordination.Member{NodeID: third.config.NodeID, Address: third.config.RaftAddress, APIAddress: third.config.PeerURL}})
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := startTestPeer(t, secondOptions)
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		state, err := third.runtime.Load().ReadState(ctx)
		cancel()
		if err == nil && len(state.Voters) == 3 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("surviving peers could not recover quorum after restart: %s/%s", restarted.runtime.Load().Status().LeaderID, third.runtime.Load().Status().LeaderID)
}

func TestClusterPeerInitializationRecoversPublishedAuthorityWithoutSidecar(t *testing.T) {
	options, installed := testPeerOptions(t, clusterPeerTestDir(t), nil)
	before, err := loadClusterPeerConfig(options.ClusterPath)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := readClusterPrivate(before.CertFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(options.ClusterPath); err != nil {
		t.Fatal(err)
	}
	path, err := prepareDesktopCluster(installed.Paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	after, err := loadClusterPeerConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := readClusterPrivate(after.CertFile)
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

func TestClusterPeerDesktopEnrollmentRunsTaskInDefaultWorkspace(t *testing.T) {
	dir := clusterPeerTestDir(t)
	bin := filepath.Join(dir, "mockagent")
	build := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build isolated ACP fixture: %v %s", err, output)
	}
	tools := filepath.Join(dir, "tools")
	if err := os.Mkdir(tools, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(bin, filepath.Join(tools, "grok")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", dir)
	options, installed := testPeerOptions(t, filepath.Join(dir, "app"), nil)
	peer := startTestPeer(t, options)
	waitPeerReady(t, peer)
	status, body := peerRequest(t, peer, http.MethodPost, "/console/desktop/agents", consoleapi.DesktopEnrollRequest{AgentIDs: []string{"grok"}})
	if status != http.StatusOK {
		t.Fatalf("register selected local fixture: %d %s", status, body)
	}
	status, body = peerRequest(t, peer, http.MethodPost, "/console/send", consoleapi.Submission{Conversation: "console:peer-integration", Input: "/project use workspace", CommandID: "choose-default-workspace"})
	if status != http.StatusOK {
		t.Fatalf("choose default workspace: %d %s", status, body)
	}
	status, body = peerRequest(t, peer, http.MethodPost, "/console/send", consoleapi.Submission{Conversation: "console:peer-integration", Input: "hello peer worker", CommandID: "first-real-worker-task"})
	var response struct {
		Reply consoleapi.Reply `json:"reply"`
	}
	if err := json.Unmarshal(body, &response); err != nil || status != http.StatusOK || !strings.Contains(response.Reply.Text, "hello peer worker") || response.Reply.Error != "" {
		t.Fatalf("first task on registered physical worker: %d %s %v", status, body, err)
	}
	declaration, err := peer.desktopDeclaration()
	if err != nil {
		t.Fatal(err)
	}
	if declaration.Agents["grok"].Node != installed.NodeID || declaration.Projects["workspace"].Home.Node != installed.NodeID {
		t.Fatal("first task did not retain explicit physical execution/workspace identity")
	}
	workerData, err := readClusterPrivate(peer.config.WorkerConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	var workerConfig node.ServerConfig
	if err := json.Unmarshal(workerData, &workerConfig); err != nil {
		t.Fatal(err)
	}
	if workerConfig.WorkspaceRoot != installed.Paths.Root {
		t.Fatalf("worker cannot reach initial desktop workspace: %s", workerConfig.WorkspaceRoot)
	}
}
