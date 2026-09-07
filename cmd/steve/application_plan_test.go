package main

import (
	"encoding/json"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/cluster"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/task"
)

func TestPlanStepHandoverUsesOriginalQuestionAndExchange(t *testing.T) {
	testPlanHandover(t, false)
}

func TestPlanningCallHandoverPreservesOriginalRequestAndExecutesItsPlan(t *testing.T) {
	testPlanHandover(t, true)
}

func testPlanHandover(t *testing.T, llm bool) {
	dir := clusterPeerTestDir(t)
	bin := filepath.Join(dir, "mockagent")
	if output, err := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent").CombinedOutput(); err != nil {
		t.Fatalf("build isolated agent: %v %s", err, output)
	}
	options, installed := testPeerOptions(t, filepath.Join(dir, "first"), nil)
	if llm {
		options.ConfigureApplication = func(cfg *config.Config, _ cluster.Activation) error {
			if _, registered := cfg.Agents["worker"]; registered {
				cfg.Gateway.Planner = "worker"
			}
			return nil
		}
	}
	peerConfig, err := loadClusterPeerConfig(options.ClusterPath)
	if err != nil {
		t.Fatal(err)
	}
	token, err := clusterRandomToken()
	if err != nil {
		t.Fatal(err)
	}
	worker := node.ServerConfig{Name: peerConfig.NodeID, Listen: "127.0.0.1:0", Token: token, Hubs: map[string]string{peerConfig.ClusterID: token}, StateDir: filepath.Join(peerConfig.DataDir, "node"), WorkspaceRoot: installed.Paths.Root, Harnesses: map[string]node.HarnessSpec{"mock": {Command: bin}}}
	if llm {
		worker.Capabilities = []string{"gpu", "internal-net", "prod-cred"}
		worker.Harnesses["mock"] = node.HarnessSpec{Command: bin, Slots: 4}
	}
	if err := saveClusterJSON(peerConfig.WorkerConfigFile, worker, true); err != nil {
		t.Fatal(err)
	}
	first := startTestPeer(t, options)
	waitPeerReady(t, first)
	secondOptions, _ := testPeerOptions(t, filepath.Join(dir, "second"), first)
	secondOptions.ConfigureApplication = options.ConfigureApplication
	second := startTestPeer(t, secondOptions)
	thirdOptions, _ := testPeerOptions(t, filepath.Join(dir, "third"), first)
	thirdOptions.ConfigureApplication = options.ConfigureApplication
	third := startTestPeer(t, thirdOptions)
	for _, peer := range []*clusterPeer{second, third} {
		if _, err := first.Join(t.Context(), coordination.JoinRequest{ID: "join-plan-" + peer.config.NodeID, Actor: "owner", Member: coordination.Member{NodeID: peer.config.NodeID, Address: peer.config.RaftAddress, APIAddress: peer.config.PeerURL}}); err != nil {
			t.Fatal(err)
		}
		if err := first.registerEnrolledWorker(t.Context(), peer.config.NodeID, "restricted"); err != nil {
			t.Fatal(err)
		}
	}
	status, body := peerRequest(t, first, http.MethodPost, "/console/agents", consoleapi.AddAgentRequest{ID: "worker", Harness: "mock", Node: first.config.NodeID})
	if status != http.StatusOK {
		t.Fatalf("register plan agent: %d %s", status, body)
	}
	if llm {
		previous := waitPeerReady(t, first)
		status, body = peerRequest(t, first, http.MethodPost, "/console/services/hub/restart", consoleapi.RestartRequest{CommandID: "apply-planner"})
		if status != http.StatusOK {
			t.Fatalf("activate registered planning agent: %d %s", status, body)
		}
		for deadline := time.Now().Add(15 * time.Second); ; {
			current := waitPeerReady(t, first)
			if current.Generation > previous.Generation {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("planner configuration did not activate")
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	conversation := "console:plan-handover"
	status, body = peerRequest(t, first, http.MethodPost, "/console/send", consoleapi.Submission{Conversation: conversation, Input: "/project use workspace", CommandID: "plan-project"})
	if status != http.StatusOK {
		t.Fatalf("bind plan project: %d %s", status, body)
	}
	status, body = peerRequest(t, first, http.MethodPost, "/console/queue", consoleapi.Submission{Conversation: conversation, Input: "/plan askme finish one inspected step", CommandID: "plan-original-input"})
	if status != http.StatusOK {
		t.Fatalf("submit plan: %d %s", status, body)
	}
	original := awaitPeerQuestion(t, first, conversation, "")
	if original.Kind == "recovery" || original.TaskID == "" || original.AttemptID == "" || !strings.HasPrefix(original.SessionID, "ns_") {
		t.Fatalf("plan lacks exact native question binding: %+v", original)
	}
	if _, err := first.runtime.Load().Transfer(t.Context(), coordination.TransferRequest{ID: "plan-handover", Actor: "owner", ExpectedEpoch: 1, TargetNodeID: second.config.NodeID}); err != nil {
		t.Fatal(err)
	}
	waitPeerReady(t, second)
	resumed := awaitPeerQuestion(t, first, conversation, original.AttemptID)
	if resumed.Kind == "recovery" || resumed.SessionID != original.SessionID || resumed.TaskID != original.TaskID || resumed.ExchangeID != original.ExchangeID {
		t.Fatalf("plan changed execution or exchange: %+v -> %+v", original, resumed)
	}
	status, body = peerRequest(t, first, http.MethodPost, "/console/questions/"+resumed.ID+"/answer", consoleapi.QuestionAnswer{CommandID: "plan-answer", Decision: "accept", Choice: "Blue"})
	if status != http.StatusOK {
		t.Fatalf("answer original step: %d %s", status, body)
	}
	for deadline := time.Now().Add(2 * time.Minute); ; {
		if llm {
			qStatus, qBody := peerRequest(t, first, http.MethodGet, "/console/questions?conversation="+conversation, nil)
			var questions struct {
				Questions []consoleapi.PendingQuestion `json:"questions"`
			}
			if qStatus == http.StatusOK && json.Unmarshal(qBody, &questions) == nil {
				for _, question := range questions.Questions {
					if question.State != "pending" || question.Kind == "recovery" {
						continue
					}
					qStatus, qBody = peerRequest(t, first, http.MethodPost, "/console/questions/"+question.ID+"/answer", consoleapi.QuestionAnswer{CommandID: "answer-" + question.ID, Decision: "accept", Choice: "Blue"})
					if qStatus != http.StatusOK {
						t.Fatalf("answer resulting step: %d %s", qStatus, qBody)
					}
				}
			}
		}
		status, body = peerRequest(t, first, http.MethodGet, "/console/queue?conversation="+conversation, nil)
		var list struct {
			Queue []consoleapi.Exchange `json:"queue"`
		}
		if status == http.StatusOK && json.Unmarshal(body, &list) == nil {
			done := false
			for _, e := range list.Queue {
				if e.ID == original.ExchangeID && e.State == "done" {
					done = true
				}
			}
			if done {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("plan did not finish original exchange: %d %s", status, body)
		}
		time.Sleep(25 * time.Millisecond)
	}
	active := waitPeerReady(t, second)
	records, err := attempt.New(active.Ledger).ForTask(t.Context(), original.TaskID)
	wantCount, wantKind, replyText := 1, attempt.KindStep, "accept:Blue"
	if llm {
		wantCount, wantKind, replyText = 4, attempt.KindPlan, "say shipped"
	}
	foundOriginal := false
	for _, record := range records {
		if record.State != attempt.Bound {
			t.Fatalf("plan execution not bound: %+v", record)
		}
		if record.ID == original.AttemptID && record.Kind == wantKind {
			foundOriginal = true
		}
	}
	if err != nil || len(records) != wantCount || !foundOriginal {
		t.Fatalf("plan replayed or failed its original step: %+v %v", records, err)
	}
	tasks, err := task.OpenLedger(active.Ledger, "")
	if err != nil {
		t.Fatal(err)
	}
	tracked, ok := tasks.Get(original.TaskID)
	if !ok || tracked.Budget.Turns != wantCount || tracked.State != task.StateDone {
		t.Fatalf("plan budget or task state differs: %+v", tracked)
	}
	status, body = peerRequest(t, first, http.MethodGet, "/console/replies?conversation="+conversation, nil)
	if status != http.StatusOK || !strings.Contains(string(body), replyText) {
		t.Fatalf("plan reply lacks original step output: %d %s", status, body)
	}
}
