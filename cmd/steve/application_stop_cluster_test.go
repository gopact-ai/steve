package main

import (
	"context"
	"errors"
	"net/http"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/task"
)

// The first coordinator loses its control RPCs after persisting the stop.
// Its healthy worker connection and native prompt continue independently.
type interruptedStopConnection struct{ nodes *node.Registry }

func (c interruptedStopConnection) Transport(node, harness string) acphost.Transport {
	return c.nodes.Transport(node, harness)
}

func (c interruptedStopConnection) NodeSession(ctx context.Context, node string, req nodewire.SessionRequest) (nodewire.SessionState, error) {
	if req.Action == "cancel" || req.Action == "abort" {
		return nodewire.SessionState{}, errors.New("previous coordinator lost stop RPC before confirmation")
	}
	return c.nodes.NodeSession(ctx, node, req)
}

func TestClusterCoordinatorCompletesStopPersistedByPreviousGeneration(t *testing.T) {
	dir := clusterPeerTestDir(t)
	bin := filepath.Join(dir, "mockagent")
	if out, err := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent").CombinedOutput(); err != nil {
		t.Fatalf("build isolated agent: %v %s", err, out)
	}
	options, installed := testPeerOptions(t, filepath.Join(dir, "first"), nil)
	installRecoveryWorker(t, options, installed.Paths.Root, bin)
	first := startTestPeer(t, options)
	active := waitPeerReady(t, first)
	secondOptions, _ := testPeerOptions(t, filepath.Join(dir, "second"), first)
	second := startTestPeer(t, secondOptions)
	thirdOptions, _ := testPeerOptions(t, filepath.Join(dir, "third"), first)
	third := startTestPeer(t, thirdOptions)
	for _, peer := range []*clusterPeer{second, third} {
		if _, err := first.Join(t.Context(), coordination.JoinRequest{ID: "join-stop-" + peer.config.NodeID, Actor: "owner", Member: coordination.Member{NodeID: peer.config.NodeID, Address: peer.config.RaftAddress, APIAddress: peer.config.PeerURL}}); err != nil {
			t.Fatal(err)
		}
		if err := first.registerEnrolledWorker(t.Context(), peer.config.NodeID, "restricted"); err != nil {
			t.Fatal(err)
		}
	}
	status, body := peerRequest(t, first, http.MethodPost, "/console/agents", consoleapi.AddAgentRequest{ID: "worker", Harness: "mock", Node: first.config.NodeID})
	if status != http.StatusOK {
		t.Fatalf("register test worker: %d %s", status, body)
	}
	conversation := "console:durable-stop"
	status, body = peerRequest(t, first, http.MethodPost, "/console/send", consoleapi.Submission{Conversation: conversation, Input: "/project use workspace", CommandID: "stop-project"})
	if status != http.StatusOK {
		t.Fatalf("bind test project: %d %s", status, body)
	}
	status, body = peerRequest(t, first, http.MethodPost, "/console/queue", consoleapi.Submission{Conversation: conversation, Input: "@worker askme original input", CommandID: "durable-stop-input"})
	if status != http.StatusOK {
		t.Fatalf("start isolated input: %d %s", status, body)
	}
	original := awaitPeerQuestion(t, first, conversation, "")
	first.mu.RLock()
	admin := first.application.Admin
	first.mu.RUnlock()
	admin.Manager.SetTransports(interruptedStopConnection{nodes: admin.Nodes})
	if _, err := admin.Tasks.SetAside(original.TaskID, task.StatePaused); err != nil {
		t.Fatal(err)
	}
	// No Registry.Stop call follows SetAside in this generation.
	if _, err := first.runtime.Load().Transfer(t.Context(), coordination.TransferRequest{ID: "move-after-stop-intent", Actor: "owner", ExpectedEpoch: active.Assignment.Epoch, TargetNodeID: second.config.NodeID}); err != nil {
		t.Fatal(err)
	}
	next := waitPeerReady(t, second)
	attempts := attempt.New(next.Ledger)
	var receipt attempt.TaskStopReceipt
	for deadline := time.Now().Add(20 * time.Second); ; {
		got, found, err := attempts.TaskStopReceipt(t.Context(), original.AttemptID)
		if err == nil && found {
			receipt = got
			break
		}
		if time.Now().After(deadline) {
			record, _ := attempts.Get(t.Context(), original.AttemptID)
			t.Fatalf("replacement coordinator never reconciled durable stop: %+v %v", record, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	st := receipt.Evidence.Session
	if st.ID != original.SessionID || st.Binding.AttemptID != original.AttemptID || st.Binding.TaskID != original.TaskID || st.InputAccepted != 1 || st.Command == nil || !st.Command.Settled {
		t.Fatalf("durable stop changed or replayed original execution: %+v", receipt)
	}
	tasks, err := task.OpenLedger(next.Ledger, "")
	if err != nil {
		t.Fatal(err)
	}
	tracked, ok := tasks.Get(original.TaskID)
	if !ok || tracked.State != task.StatePaused || tracked.Budget.Turns != 1 {
		t.Fatalf("stop reconciliation changed user task authority: %+v", tracked)
	}
}
