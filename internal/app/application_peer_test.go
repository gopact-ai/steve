package app

import (
	"context"
	"encoding/json"
	"fmt"
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
	adminsvc "github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/cluster"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/readmodel"
)

func assertPeerUsage(t *testing.T, peer *cluster.Peer) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, peer.UiURL+"/usage", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+peer.UIToken)
	client := &http.Client{Timeout: peerRequestTimeout}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "application/json" || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("usage gateway returned status=%d type=%q cache=%q", response.StatusCode, response.Header.Get("Content-Type"), response.Header.Get("Cache-Control"))
	}
	var usage readmodel.UsageSnapshot
	if err := json.NewDecoder(response.Body).Decode(&usage); err != nil {
		t.Fatal(err)
	}
	if usage.Usage == nil || usage.At.IsZero() || len(usage.Sources) != 1 || !usage.Sources[0].Wired || usage.Sources[0].Error != "" {
		t.Fatalf("usage unavailable through desktop gateway: %+v", usage)
	}
	for _, period := range []string{"1d", "7d", "30d"} {
		if _, ok := usage.Usage.Periods[period]; !ok {
			t.Fatalf("usage missing period %s", period)
		}
	}
}

func TestClusterPeerActualApplicationActivates(t *testing.T) {
	options, _ := testPeerOptions(t, ClusterPeerTestDir(t), nil)
	peer := StartTestPeer(t, options)
	WaitPeerReady(t, peer)
	status, body := PeerRequest(t, peer, http.MethodGet, "/state", nil)
	if status != http.StatusOK {
		t.Fatalf("actual application failed: %d %s", status, body)
	}
	assertPeerUsage(t, peer)
	if err := peer.Close(); err != nil {
		t.Fatal(fmt.Errorf("shutdown actual application: %w", err))
	}
}

func TestClusterPeerActualApplicationsRebuildAcrossThreePeerTransfer(t *testing.T) {
	options, _ := testPeerOptions(t, ClusterPeerTestDir(t), nil)
	var firstStarts, secondStarts atomic.Int32
	options.ApplicationReady = func(*adminsvc.Service, cluster.ApplicationServer, cluster.Activation) error {
		firstStarts.Add(1)
		return nil
	}
	first := StartTestPeer(t, options)
	WaitPeerReady(t, first)
	secondOptions, _ := testPeerOptions(t, ClusterPeerTestDir(t), first)
	secondOptions.ApplicationReady = func(*adminsvc.Service, cluster.ApplicationServer, cluster.Activation) error {
		secondStarts.Add(1)
		return nil
	}
	second := StartTestPeer(t, secondOptions)
	thirdOptions, _ := testPeerOptions(t, ClusterPeerTestDir(t), first)
	third := StartTestPeer(t, thirdOptions)
	for _, peer := range []*cluster.Peer{second, third} {
		_, err := first.Join(context.Background(), coordination.JoinRequest{ID: "real-join-" + peer.Config.NodeID, Actor: "owner", Member: coordination.Member{NodeID: peer.Config.NodeID, Address: peer.Config.RaftAddress, APIAddress: peer.Config.PeerURL, Voting: true}})
		if err != nil {
			t.Fatal(err)
		}
	}
	status, body := PeerRequest(t, first, http.MethodPost, "/console/coordination/transfer", consoleapi.CoordinatorTransfer{CommandID: "real-transfer", ExpectedEpoch: 1, TargetNodeID: second.Config.NodeID})
	if status != http.StatusOK {
		t.Fatalf("real application transfer: %d %s", status, body)
	}
	WaitPeerReady(t, second)
	status, body = PeerRequest(t, first, http.MethodGet, "/state", nil)
	if status != http.StatusOK {
		t.Fatalf("original UI could not access second real application: %d %s", status, body)
	}
	assertPeerUsage(t, first)
	status, body = PeerRequest(t, first, http.MethodPost, "/console/coordination/transfer", consoleapi.CoordinatorTransfer{CommandID: "real-return", ExpectedEpoch: 2, TargetNodeID: first.Config.NodeID})
	if status != http.StatusOK {
		t.Fatalf("real application return: %d %s", status, body)
	}
	WaitPeerReady(t, first)
	if firstStarts.Load() != 2 || secondStarts.Load() != 1 {
		t.Fatalf("real application stores not reconstructed: %d/%d", firstStarts.Load(), secondStarts.Load())
	}
}

func TestClusterPeerDesktopEnrollmentRunsTaskInDefaultWorkspace(t *testing.T) {
	dir := ClusterPeerTestDir(t)
	bin := filepath.Join(dir, "mockagent")
	build := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build isolated ACP fixture: %v %s", err, output)
	}
	t.Setenv("PATH", piTools(t, dir)+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", dir)
	options, installed := testPeerOptions(t, filepath.Join(dir, "app"), nil)
	// The worker's pinned pi-acp is already cached; it is the mock agent.
	cachePeerAdapter(t, options, "pi-acp", "#!/bin/sh\nexec '"+bin+"' \"$@\"\n")
	peer := StartTestPeer(t, options)
	WaitPeerReady(t, peer)
	status, body := PeerRequest(t, peer, http.MethodPost, "/console/desktop/agents", consoleapi.DesktopEnrollRequest{AgentIDs: []string{"pi"}})
	if status != http.StatusOK {
		t.Fatalf("register selected local fixture: %d %s", status, body)
	}
	status, body = PeerRequest(t, peer, http.MethodPost, "/console/send", consoleapi.Submission{Conversation: "console:peer-integration", Input: "/project use workspace", CommandID: "choose-default-workspace"})
	if status != http.StatusOK {
		t.Fatalf("choose default workspace: %d %s", status, body)
	}
	status, body = PeerRequest(t, peer, http.MethodPost, "/console/send", consoleapi.Submission{Conversation: "console:peer-integration", Input: "hello peer worker", CommandID: "first-real-worker-task"})
	var response struct {
		Reply consoleapi.Reply `json:"reply"`
	}
	if err := json.Unmarshal(body, &response); err != nil || status != http.StatusOK || !strings.Contains(response.Reply.Text, "hello peer worker") || response.Reply.Error != "" {
		t.Fatalf("first task on registered physical worker: %d %s %v", status, body, err)
	}
	declaration, err := peer.DesktopDeclaration()
	if err != nil {
		t.Fatal(err)
	}
	if declaration.Agents["pi"].Node != installed.NodeID || declaration.Projects["workspace"].Home.Node != installed.NodeID {
		t.Fatal("first task did not retain explicit physical execution/workspace identity")
	}
	workerData, err := cluster.ReadClusterPrivate(peer.Config.WorkerConfigFile)
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

// The guide sends the name the owner typed, what the agent is for and
// which one a conversation starts with. A peer that only read the older
// list of tool IDs registered nothing and still answered OK, which the
// guide could only report as "registration unconfirmed".
func TestClusterPeerDesktopEnrollmentKeepsTheNameTheOwnerChose(t *testing.T) {
	dir := ClusterPeerTestDir(t)
	bin := filepath.Join(dir, "mockagent")
	build := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build isolated ACP fixture: %v %s", err, output)
	}
	t.Setenv("PATH", piTools(t, dir)+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", dir)
	options, installed := testPeerOptions(t, filepath.Join(dir, "app"), nil)
	// The worker's pinned pi-acp is already cached; it is the mock agent.
	cachePeerAdapter(t, options, "pi-acp", "#!/bin/sh\nexec '"+bin+"' \"$@\"\n")
	peer := StartTestPeer(t, options)
	WaitPeerReady(t, peer)
	request := consoleapi.DesktopEnrollRequest{Agents: []consoleapi.DesktopEnrollAgent{{CandidateID: "pi", AgentID: "scout", About: "代码审查", Default: true}}}
	status, body := PeerRequest(t, peer, http.MethodPost, "/console/desktop/agents", request)
	if status != http.StatusOK {
		t.Fatalf("register the chosen local tool: %d %s", status, body)
	}
	var result consoleapi.DesktopStatus
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	if result.AgentCount != 1 || result.LocalAgentCount != 1 || result.DefaultAgent != "scout" {
		t.Fatalf("enrollment answered without the registration it made: %s", body)
	}
	declaration, err := peer.DesktopDeclaration()
	if err != nil {
		t.Fatal(err)
	}
	agent, ok := declaration.Agents["scout"]
	if !ok {
		t.Fatalf("the chosen name was not registered: %v", declaration.Agents)
	}
	if agent.Node != installed.NodeID || agent.Harness != "pi" || agent.About != "代码审查" || !agent.Default {
		t.Fatalf("placement, use or default lost in enrollment: %+v", agent)
	}
	if _, taken := declaration.Agents["pi"]; taken {
		t.Fatal("a renamed agent must not also hold the tool's own ID")
	}
}

// A business application that cannot be built on this node is this node's
// to retry: the replica stays in the cluster, the generation stays inactive
// with the reason readable, and it starts once the cause is gone, without
// the peer stopping.
func TestClusterPeerKeepsItsReplicaWhileItsApplicationCannotBeBuilt(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = taken.Close() })
	addr := taken.Addr().String()
	options, installed := testPeerOptions(t, ClusterPeerTestDir(t), nil)
	cfg, err := config.Load(installed.Paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Gateway.IssuerAddr = addr
	cfg.Gateway.IssuerToken = "issuer-token"
	if err := config.Save(installed.Paths.Config, cfg); err != nil {
		t.Fatal(err)
	}
	peer := StartTestPeer(t, options)
	runtime := peer.Runtime.Load()
	deadline := time.Now().Add(20 * time.Second)
	for !strings.Contains(runtime.Status().LastError, "lease issuer on "+addr) {
		if time.Now().After(deadline) {
			t.Fatalf("the application was not refused its lease issuer; status=%+v", runtime.Status())
		}
		time.Sleep(25 * time.Millisecond)
	}
	// The next build starts only after a wait, long after the runner of the
	// one that failed has done whatever it does to the runtime.
	generation := runtime.Status().Generation
	for runtime.Status().Generation == generation && !runtime.Status().Closed {
		if time.Now().After(deadline) {
			t.Fatalf("the application was not built again; status=%+v", runtime.Status())
		}
		time.Sleep(25 * time.Millisecond)
	}
	if status := runtime.Status(); status.Closed {
		t.Fatalf("a build that failed ended the runtime; status=%+v", status)
	}
	select {
	case err := <-peer.Errors:
		t.Fatalf("a build that failed stopped the peer: %v", err)
	default:
	}
	if err := taken.Close(); err != nil {
		t.Fatal(err)
	}
	WaitPeerReady(t, peer)
}

// piTools is a directory holding pi and the Node.js runtime pi-acp needs.
func piTools(t *testing.T, dir string) string {
	t.Helper()
	tools := filepath.Join(dir, "tools")
	if err := os.MkdirAll(tools, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pi", "node", "npm"} {
		if err := os.WriteFile(filepath.Join(tools, name), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return tools
}

// cachePeerAdapter places a verified installation of the pinned adapter in
// the peer's worker cache, so enrollment reaches no registry.
func cachePeerAdapter(t *testing.T, options cluster.PeerOptions, name, script string) {
	t.Helper()
	cfg, err := cluster.LoadClusterPeerConfig(options.ClusterPath)
	if err != nil {
		t.Fatal(err)
	}
	pin := adapter.Catalog[name]
	installed := filepath.Join(cfg.DataDir, "node", "adapters", name+"@"+pin.Version)
	command := filepath.Join(installed, pin.Bin)
	if err := os.MkdirAll(filepath.Dir(command), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(command, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	marker, _ := json.Marshal(map[string]string{"package": pin.Package, "version": pin.Version, "integrity": pin.Integrity})
	if err := os.WriteFile(filepath.Join(installed, ".steve-adapter.json"), marker, 0o600); err != nil {
		t.Fatal(err)
	}
}
