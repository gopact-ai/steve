package app

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/cluster"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
)

func TestAutomaticCoordinatorLossPreservesHealthyTaskAndReturningDesktop(t *testing.T) {
	dir := ClusterPeerTestDir(t)
	bin := filepath.Join(dir, "mockagent")
	if output, err := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent").CombinedOutput(); err != nil {
		t.Fatalf("build isolated agent: %v %s", err, output)
	}
	firstOptions, _ := testPeerOptions(t, filepath.Join(dir, "first"), nil)
	firstOptions.AllowAutoFailover = true
	first := StartTestPeer(t, firstOptions)
	WaitPeerReady(t, first)
	originalURL := first.UiURL
	secondOptions, _ := testPeerOptions(t, filepath.Join(dir, "second"), first)
	secondOptions.AllowAutoFailover = true
	second := StartTestPeer(t, secondOptions)
	thirdOptions, thirdInstallation := testPeerOptions(t, filepath.Join(dir, "third"), first)
	thirdOptions.AllowAutoFailover = true
	thirdConfig, err := cluster.LoadClusterPeerConfig(thirdOptions.ClusterPath)
	if err != nil {
		t.Fatal(err)
	}
	token, err := cluster.ClusterRandomToken()
	if err != nil {
		t.Fatal(err)
	}
	worker := node.ServerConfig{Name: thirdConfig.NodeID, Listen: "127.0.0.1:0", Token: token, Hubs: map[string]string{thirdConfig.ClusterID: token}, StateDir: filepath.Join(thirdConfig.DataDir, "node"), WorkspaceRoot: thirdInstallation.Paths.Root, Harnesses: map[string]node.HarnessSpec{"mock": {Command: bin}}}
	if err := cluster.SaveClusterJSON(thirdConfig.WorkerConfigFile, worker, true); err != nil {
		t.Fatal(err)
	}
	third := StartTestPeer(t, thirdOptions)
	for _, peer := range []*cluster.Peer{second, third} {
		if _, err := first.Join(t.Context(), coordination.JoinRequest{ID: "join-auto-" + peer.Config.NodeID, Actor: "owner", Member: coordination.Member{NodeID: peer.Config.NodeID, Address: peer.Config.RaftAddress, APIAddress: peer.Config.PeerURL}}); err != nil {
			t.Fatal(err)
		}
		if err := first.RegisterEnrolledWorker(t.Context(), peer.Config.NodeID, "restricted"); err != nil {
			t.Fatal(err)
		}
		current, err := first.Runtime.Load().ReadState(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := first.SetCoordinatorEligibility(t.Context(), consoleapi.CoordinatorEligibility{CommandID: "eligible-" + peer.Config.NodeID, ExpectedRevision: current.Revision, NodeID: peer.Config.NodeID, Eligible: true}); err != nil {
			t.Fatal(err)
		}
	}
	current, err := first.Runtime.Load().ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.SetAutoFailover(t.Context(), consoleapi.CoordinatorPolicy{CommandID: "enable-auto", ExpectedRevision: current.Revision, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	workdir := filepath.Join(thirdInstallation.Paths.Root, "remote-work")
	if err := os.MkdirAll(workdir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workdir, "README.md"), []byte("isolated task project\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	status, body := PeerRequest(t, first, http.MethodPost, "/console/projects", consoleapi.AddProjectRequest{ID: "remote-work", Node: third.Config.NodeID, Path: workdir, Repo: "inplace", Level: "internal"})
	if status != http.StatusOK {
		t.Fatalf("create remote project: %d %s", status, body)
	}
	status, body = PeerRequest(t, first, http.MethodPost, "/console/agents", consoleapi.AddAgentRequest{ID: "worker", Node: third.Config.NodeID, Harness: "mock"})
	if status != http.StatusOK {
		t.Fatalf("register remote agent: %d %s", status, body)
	}
	conversation := "console:auto-recovery"
	status, body = PeerRequest(t, first, http.MethodPost, "/console/send", consoleapi.Submission{Conversation: conversation, Input: "/project use remote-work", CommandID: "bind-remote"})
	if status != http.StatusOK {
		t.Fatalf("bind remote project: %d %s", status, body)
	}
	status, body = PeerRequest(t, first, http.MethodPost, "/console/queue", consoleapi.Submission{Conversation: conversation, Input: "askme keep this exact task running", CommandID: "auto-original-input"})
	if status != http.StatusOK {
		t.Fatalf("start remote execution: %d %s", status, body)
	}
	original := awaitPeerQuestion(t, first, conversation, "")
	active := WaitPeerReady(t, first)
	sessions, err := state.OpenLedger(active.Ledger, "")
	if err != nil {
		t.Fatal(err)
	}
	grant := sessions.Conversation(conversation).Sessions["worker"].AgentToken
	first.Mu.RLock()
	registry := first.Application.Admin.Nodes
	first.Mu.RUnlock()
	mcpURL, err := registry.MCPEndpoint(t.Context(), third.Config.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	checkRetainedMCP(t, mcpURL, grant, http.StatusOK)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	var replacement *cluster.Peer
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	for replacement == nil {
		state, readErr := second.Runtime.Load().ReadState(ctx)
		if readErr == nil && state.Coordinator.Epoch > active.Assignment.Epoch && state.Coordinator.NodeID != first.Config.NodeID {
			for _, peer := range []*cluster.Peer{second, third} {
				if peer.Config.NodeID == state.Coordinator.NodeID {
					replacement = peer
				}
			}
		}
		if ctx.Err() != nil {
			t.Fatalf("surviving majority did not take over: %v", readErr)
		}
		if replacement == nil {
			time.Sleep(25 * time.Millisecond)
		}
	}
	WaitPeerReady(t, replacement)
	resumed := awaitPeerQuestion(t, replacement, conversation, original.AttemptID)
	if resumed.Kind == "recovery" || resumed.TaskID != original.TaskID || resumed.ExchangeID != original.ExchangeID || resumed.SessionID != original.SessionID {
		t.Fatalf("automatic takeover changed original execution: %+v -> %+v", original, resumed)
	}
	checkRetainedMCP(t, mcpURL, grant, http.StatusOK)
	status, body = PeerRequest(t, replacement, http.MethodPost, "/console/questions/"+resumed.ID+"/answer", consoleapi.QuestionAnswer{CommandID: "auto-answer", Decision: "accept", Choice: "Blue"})
	if status != http.StatusOK {
		t.Fatalf("answer after automatic takeover: %d %s", status, body)
	}
	for deadline := time.Now().Add(25 * time.Second); ; {
		status, body = PeerRequest(t, replacement, http.MethodGet, "/console/queue?conversation="+conversation, nil)
		var listing struct {
			Queue []consoleapi.Exchange `json:"queue"`
		}
		if status == http.StatusOK && json.Unmarshal(body, &listing) == nil {
			finished := false
			for _, entry := range listing.Queue {
				if entry.ID == original.ExchangeID && entry.State == "done" {
					finished = true
				}
			}
			if finished {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("original exchange did not finish: %d %s", status, body)
		}
		time.Sleep(25 * time.Millisecond)
	}
	latest := WaitPeerReady(t, replacement)
	records, err := attempt.New(latest.Ledger).ForTask(t.Context(), original.TaskID)
	if err != nil || len(records) != 1 || records[0].State != attempt.Bound || records[0].Node != third.Config.NodeID {
		t.Fatalf("automatic takeover replayed or moved execution: %+v %v", records, err)
	}
	tasks, err := task.OpenLedger(latest.Ledger, "")
	if err != nil {
		t.Fatal(err)
	}
	tracked, ok := tasks.Get(original.TaskID)
	if !ok || tracked.Budget.Turns != 1 {
		t.Fatalf("automatic takeover charged another turn: %+v", tracked)
	}
	returned := StartTestPeer(t, firstOptions)
	if returned.UiURL != originalURL {
		t.Fatal("returning desktop changed its stable origin")
	}
	status, body = PeerRequest(t, returned, http.MethodGet, "/console/coordination", nil)
	var view consoleapi.CoordinationView
	if status != http.StatusOK || json.Unmarshal(body, &view) != nil || view.CoordinatorID != replacement.Config.NodeID {
		t.Fatalf("returning desktop did not locate current coordinator: %d %s", status, body)
	}
	transferred := false
	for _, event := range view.Events {
		if event.Kind == "coordinator_transferred" && event.From == first.Config.NodeID && event.To == replacement.Config.NodeID && event.Reason == "coordinator_unreachable" && event.Actor == "system" {
			transferred = true
		}
	}
	if !transferred {
		t.Fatalf("automatic handover missing from audit: %s", body)
	}
	status, body = PeerRequest(t, returned, http.MethodGet, "/console/replies?conversation="+conversation, nil)
	if status != http.StatusOK || !strings.Contains(string(body), "accept:Blue") {
		t.Fatalf("returning desktop lost subsequent work: %d %s", status, body)
	}
}
