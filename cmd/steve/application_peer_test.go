package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	adminsvc "github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/cluster"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/node"
)

func TestClusterPeerActualApplicationActivates(t *testing.T) {
	options, _ := testPeerOptions(t, ClusterPeerTestDir(t), nil)
	peer := StartTestPeer(t, options)
	WaitPeerReady(t, peer)
	status, body := PeerRequest(t, peer, http.MethodGet, "/state", nil)
	if status != http.StatusOK {
		t.Fatalf("actual application failed: %d %s", status, body)
	}
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
		_, err := first.Join(context.Background(), coordination.JoinRequest{ID: "real-join-" + peer.Config.NodeID, Actor: "owner", Member: coordination.Member{NodeID: peer.Config.NodeID, Address: peer.Config.RaftAddress, APIAddress: peer.Config.PeerURL}})
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
	peer := StartTestPeer(t, options)
	WaitPeerReady(t, peer)
	status, body := PeerRequest(t, peer, http.MethodPost, "/console/desktop/agents", consoleapi.DesktopEnrollRequest{AgentIDs: []string{"grok"}})
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
	if declaration.Agents["grok"].Node != installed.NodeID || declaration.Projects["workspace"].Home.Node != installed.NodeID {
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
