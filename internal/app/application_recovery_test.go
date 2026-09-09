package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
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
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
)

func awaitPeerQuestion(t *testing.T, peer *cluster.Peer, conversation, attemptID string) consoleapi.PendingQuestion {
	t.Helper()
	var last []byte
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); {
		status, body := PeerRequest(t, peer, http.MethodGet, "/console/questions?conversation="+conversation, nil)
		last = body
		if status == http.StatusOK {
			var response struct {
				Questions []consoleapi.PendingQuestion `json:"questions"`
			}
			if err := json.Unmarshal(body, &response); err != nil {
				t.Fatal(err)
			}
			for _, q := range response.Questions {
				if q.State == "pending" && (attemptID == "" || q.AttemptID == attemptID) {
					return q
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("original node question was not attached: %s", last)
	return consoleapi.PendingQuestion{}
}

func TestThreePeerCoordinatorTransferResumesOriginalNodeCommandAndExchange(t *testing.T) {
	dir := ClusterPeerTestDir(t)
	bin := filepath.Join(dir, "mockagent")
	build := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build test ACP agent: %v %s", err, output)
	}
	options, installed := testPeerOptions(t, filepath.Join(dir, "first"), nil)
	peerConfig, err := cluster.LoadClusterPeerConfig(options.ClusterPath)
	if err != nil {
		t.Fatal(err)
	}
	token, err := cluster.ClusterRandomToken()
	if err != nil {
		t.Fatal(err)
	}
	worker := node.ServerConfig{Name: peerConfig.NodeID, Listen: "127.0.0.1:0", Token: token, Hubs: map[string]string{peerConfig.ClusterID: token}, StateDir: filepath.Join(peerConfig.DataDir, "node"), WorkspaceRoot: installed.Paths.Root, Harnesses: map[string]node.HarnessSpec{"mock": {Command: bin}}}
	if err := cluster.SaveClusterJSON(peerConfig.WorkerConfigFile, worker, true); err != nil {
		t.Fatal(err)
	}
	first := StartTestPeer(t, options)
	WaitPeerReady(t, first)
	secondOptions, _ := testPeerOptions(t, filepath.Join(dir, "second"), first)
	second := StartTestPeer(t, secondOptions)
	thirdOptions, _ := testPeerOptions(t, filepath.Join(dir, "third"), first)
	third := StartTestPeer(t, thirdOptions)
	for _, peer := range []*cluster.Peer{second, third} {
		if _, err := first.Join(t.Context(), coordination.JoinRequest{ID: "join-recovery-" + peer.Config.NodeID, Actor: "owner", Member: coordination.Member{NodeID: peer.Config.NodeID, Address: peer.Config.RaftAddress, APIAddress: peer.Config.PeerURL}}); err != nil {
			t.Fatal(err)
		}
		if err := first.RegisterEnrolledWorker(t.Context(), peer.Config.NodeID, "restricted"); err != nil {
			t.Fatal(err)
		}
	}
	status, body := PeerRequest(t, first, http.MethodPost, "/console/agents", consoleapi.AddAgentRequest{ID: "worker", Harness: "mock", Node: first.Config.NodeID})
	if status != http.StatusOK {
		t.Fatalf("register fixture agent: %d %s", status, body)
	}
	pluginFixture := preparePeerPluginFixture(t, first)
	pluginFixture.activate(t, first, "1.0.0")
	adoptPeerPluginAgent(t, first, first.Config.NodeID)
	conversation := "console:retained-integration"
	status, body = PeerRequest(t, first, http.MethodPost, "/console/send", consoleapi.Submission{Conversation: conversation, Input: "/project use workspace", CommandID: "bind-recovery-workspace"})
	if status != http.StatusOK {
		t.Fatalf("bind workspace: %d %s", status, body)
	}
	status, body = PeerRequest(t, first, http.MethodPost, "/console/queue", consoleapi.Submission{Conversation: conversation, Input: "askme plugincheck original retained command", CommandID: "original-retained-input"})
	if status != http.StatusOK {
		t.Fatalf("submit original command: %d %s", status, body)
	}
	var submitted consoleapi.Exchange
	if err := json.Unmarshal(body, &submitted); err != nil {
		t.Fatal(err)
	}
	original := awaitPeerQuestion(t, first, conversation, "")
	if submitted.ID == "" || original.ExchangeID != submitted.ID {
		t.Fatalf("question was not attached to the submitted exchange: %+v %+v", submitted, original)
	}
	if original.AttemptID == "" || original.TaskID == "" || !strings.HasPrefix(original.SessionID, "ns_") {
		t.Fatalf("original question lacks retained execution binding: %+v", original)
	}
	firstActive := WaitPeerReady(t, first)
	conversationStore, err := state.OpenLedger(firstActive.Ledger, "")
	if err != nil {
		t.Fatal(err)
	}
	savedPlugin := conversationStore.Conversation(conversation).Sessions["worker"].PluginRuntime
	if savedPlugin == nil {
		t.Fatal("plugin binding missing before transfer")
	}
	pluginFixture.activate(t, first, "2.0.0")
	agentToken := conversationStore.Conversation(conversation).Sessions["worker"].AgentToken
	first.Mu.RLock()
	admin := first.Application.Admin
	first.Mu.RUnlock()
	mcpURL, err := admin.Nodes.MCPEndpoint(t.Context(), first.Config.NodeID)
	if err != nil || agentToken == "" {
		t.Fatalf("native collaboration binding missing: %v", err)
	}
	checkRetainedMCP(t, mcpURL, agentToken, http.StatusOK)
	if _, err := first.Runtime.Load().Transfer(t.Context(), coordination.TransferRequest{ID: "move-live-coordinator", Actor: "owner", ExpectedEpoch: 1, TargetNodeID: second.Config.NodeID, Reason: "user"}); err != nil {
		t.Fatal(err)
	}
	WaitPeerReady(t, second)
	resumed := awaitPeerQuestion(t, first, conversation, original.AttemptID)
	checkRetainedMCP(t, mcpURL, agentToken, http.StatusOK)
	if resumed.Kind == "recovery" {
		t.Fatalf("live node could not reattach: %+v", resumed)
	}
	if resumed.TaskID != original.TaskID || resumed.ExchangeID != original.ExchangeID || resumed.SessionID != original.SessionID {
		t.Fatalf("reattach changed task/session/exchange: original=%+v resumed=%+v", original, resumed)
	}
	choice := ""
	for _, option := range resumed.Options {
		if option.ID == "Blue" {
			choice = option.ID
		}
	}
	if choice == "" {
		t.Fatalf("original choices disappeared: %+v", resumed.Options)
	}
	status, body = PeerRequest(t, first, http.MethodPost, "/console/questions/"+resumed.ID+"/answer", consoleapi.QuestionAnswer{CommandID: "answer-original-colour", Decision: "accept", Choice: choice})
	if status != http.StatusOK {
		t.Fatalf("answer retained question: %d %s", status, body)
	}
	var finished consoleapi.Exchange
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); {
		status, body = PeerRequest(t, first, http.MethodGet, "/console/queue?conversation="+conversation, nil)
		if status == http.StatusOK {
			var listing struct {
				Queue []consoleapi.Exchange `json:"queue"`
			}
			if err := json.Unmarshal(body, &listing); err != nil {
				t.Fatal(err)
			}
			for _, e := range listing.Queue {
				if e.ID == original.ExchangeID && (e.State == "done" || e.State == "failed") {
					finished = e
				}
			}
		}
		if finished.ID != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if finished.State != "done" || finished.ReplyID == "" {
		t.Fatalf("retained exchange did not finish: %+v %s", finished, body)
	}
	active := WaitPeerReady(t, second)
	book := active.Ledger
	attempts := attempt.New(book)
	record, err := attempts.Get(t.Context(), original.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != attempt.Bound || record.Result == nil || !strings.Contains(string(record.Result.Output), "accept:Blue") {
		t.Fatalf("original attempt lacks atomic completed response: %+v", record)
	}
	if record.PluginRuntimeID() != savedPlugin.ID || !strings.Contains(string(record.Result.Output), "PLUGIN_SKILL_github_1.0.0") || strings.Contains(string(record.Result.Output), "PLUGIN_SKILL_github_2.0.0") || !strings.Contains(string(record.Result.Output), "REVIEW_EVIDENCE/team-tools/1.0.0") || pluginFixture.calls.Load() != 2 {
		t.Fatalf("plugin transfer changed content or replayed tools: %s calls=%d", record.Result.Output, pluginFixture.calls.Load())
	}
	records, err := attempts.ForTask(t.Context(), original.TaskID)
	if err != nil || len(records) != 1 {
		t.Fatalf("recovery created a second attempt: %+v %v", records, err)
	}
	tasks, err := task.OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	tracked, ok := tasks.Get(original.TaskID)
	if !ok || len(tracked.Attempts) != 1 || tracked.Budget.Turns != 1 {
		t.Fatalf("recovery charged another task turn: %+v", tracked)
	}
	second.Mu.RLock()
	registry := second.Application.Admin.Nodes
	second.Mu.RUnlock()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	state, err := registry.NodeSession(ctx, first.Worker().Name, nodewire.SessionRequest{Action: "attach", ID: record.Session, Authority: nodewire.SessionAuthority{ClusterID: first.Config.ClusterID, CoordinatorNodeID: active.NodeID, CoordinatorEpoch: active.Assignment.Epoch, WriterGeneration: active.WriterGeneration}, Binding: nodewire.SessionBinding{PluginRuntimeID: record.PluginRuntimeID(), ProjectID: record.Project, SessionID: attempt.RetainedSessionID(tracked.Channel, tracked.ID, record.Agent), TaskID: record.TaskID, AttemptID: record.ID, NodeID: record.Node, ExecutionEpoch: attempt.SessionExecutionEpoch(record), TaskEpoch: record.Execution.Epoch}, CommandID: record.TurnID})
	if err != nil || state.InputAccepted != 1 || state.Command == nil || state.Command.InputSequence != 1 {
		t.Fatalf("original prompt was replayed or receipt lost: %+v %v", state, err)
	}
	status, body = PeerRequest(t, first, http.MethodPost, "/console/send", consoleapi.Submission{Conversation: conversation, Input: "/new", CommandID: "clear-retained-session"})
	if status != http.StatusOK {
		t.Fatalf("clear completed retained session: %d %s", status, body)
	}
	closeCtx, finishClose := context.WithTimeout(t.Context(), 5*time.Second)
	defer finishClose()
	closed, err := registry.NodeSession(closeCtx, first.Worker().Name, nodewire.SessionRequest{Action: "attach", ID: record.Session, Authority: nodewire.SessionAuthority{ClusterID: first.Config.ClusterID, CoordinatorNodeID: active.NodeID, CoordinatorEpoch: active.Assignment.Epoch, WriterGeneration: active.WriterGeneration}, Binding: state.Binding, CommandID: attempt.InputCommandID(record)})
	if err != nil || closed.State != "closed" {
		t.Fatalf("new conversation did not close the original native session: %+v %v; response=%s", closed, err, body)
	}
	checkRetainedMCP(t, mcpURL, agentToken, http.StatusUnauthorized)
	checkPeerPluginProjectIsolation(t, second, first)
	checkPeerPluginRollbackAndRemoval(t, second, conversation)
}

func checkRetainedMCP(t *testing.T, address, token string, want int) {
	t.Helper()
	payload := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"steve_context","arguments":{}}}`)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, address, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != want {
		t.Fatalf("retained collaboration endpoint: status=%d want=%d body=%s err=%v", response.StatusCode, want, body, err)
	}
	if want == http.StatusOK {
		var value struct {
			Result struct {
				IsError bool `json:"isError"`
			} `json:"result"`
			Error json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(body, &value); err != nil || len(value.Error) != 0 || value.Result.IsError {
			t.Fatalf("retained tool call failed: %s %v", body, err)
		}
	}
}

func installRecoveryWorker(t *testing.T, options cluster.PeerOptions, root, bin string) {
	t.Helper()
	cfg, err := cluster.LoadClusterPeerConfig(options.ClusterPath)
	if err != nil {
		t.Fatal(err)
	}
	token, err := cluster.ClusterRandomToken()
	if err != nil {
		t.Fatal(err)
	}
	worker := node.ServerConfig{Name: cfg.NodeID, Listen: "127.0.0.1:0", Token: token, Hubs: map[string]string{cfg.ClusterID: token}, StateDir: filepath.Join(cfg.DataDir, "node"), WorkspaceRoot: root, Harnesses: map[string]node.HarnessSpec{"mock": {Command: bin}}}
	if err := cluster.SaveClusterJSON(cfg.WorkerConfigFile, worker, true); err != nil {
		t.Fatal(err)
	}
}

func TestSourceNodeLossUsesPlanScopedApprovalAndContinuesInIsolatedWorkspace(t *testing.T) {
	dir := ClusterPeerTestDir(t)
	bin := filepath.Join(dir, "mockagent")
	if out, err := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent").CombinedOutput(); err != nil {
		t.Fatalf("build agent: %v %s", err, out)
	}
	options, installed := testPeerOptions(t, filepath.Join(dir, "source"), nil)
	installRecoveryWorker(t, options, installed.Paths.Root, bin)
	first := StartTestPeer(t, options)
	WaitPeerReady(t, first)
	secondOptions, secondInstall := testPeerOptions(t, filepath.Join(dir, "target"), first)
	installRecoveryWorker(t, secondOptions, secondInstall.Paths.Root, bin)
	second := StartTestPeer(t, secondOptions)
	thirdOptions, thirdInstall := testPeerOptions(t, filepath.Join(dir, "third"), first)
	installRecoveryWorker(t, thirdOptions, thirdInstall.Paths.Root, bin)
	third := StartTestPeer(t, thirdOptions)
	for _, peer := range []*cluster.Peer{second, third} {
		if _, err := first.Join(t.Context(), coordination.JoinRequest{ID: "join-loss-" + peer.Config.NodeID, Actor: "owner", Member: coordination.Member{NodeID: peer.Config.NodeID, Address: peer.Config.RaftAddress, APIAddress: peer.Config.PeerURL}}); err != nil {
			t.Fatal(err)
		}
		if err := first.RegisterEnrolledWorker(t.Context(), peer.Config.NodeID, "restricted"); err != nil {
			t.Fatal(err)
		}
	}
	for _, spec := range []consoleapi.AddAgentRequest{{ID: "worker", Harness: "mock", Node: first.Config.NodeID}, {ID: "backup", Harness: "mock", Node: second.Config.NodeID}} {
		status, body := PeerRequest(t, first, http.MethodPost, "/console/agents", spec)
		if status != http.StatusOK {
			t.Fatalf("register agent: %d %s", status, body)
		}
	}
	pluginFixture := preparePeerPluginFixture(t, first, second, third)
	pluginFixture.activate(t, first, "1.0.0")
	adoptPeerPluginAgent(t, first, first.Config.NodeID)
	conversation := "console:source-loss"
	status, body := PeerRequest(t, first, http.MethodPost, "/console/send", consoleapi.Submission{Conversation: conversation, Input: "/project use workspace", CommandID: "source-loss-project"})
	if status != http.StatusOK {
		t.Fatalf("bind project: %d %s", status, body)
	}
	status, body = PeerRequest(t, first, http.MethodPost, "/console/queue", consoleapi.Submission{Conversation: conversation, Input: "@worker askme plugincheck source lost", CommandID: "source-loss-input"})
	if status != http.StatusOK {
		t.Fatalf("submit: %d %s", status, body)
	}
	original := awaitPeerQuestion(t, first, conversation, "")
	if original.Kind == "recovery" {
		t.Fatalf("original task failed: %+v", original)
	}
	oldRecord, err := attempt.New(first.Runtime.Load().Ledger()).Get(t.Context(), original.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if oldRecord.PluginRuntime == nil {
		t.Fatal("plugin source binding absent")
	}
	pluginFixture.activate(t, first, "2.0.0")
	if _, err := first.Runtime.Load().Transfer(t.Context(), coordination.TransferRequest{ID: "coordinate-away", Actor: "owner", ExpectedEpoch: 1, TargetNodeID: second.Config.NodeID}); err != nil {
		t.Fatal(err)
	}
	WaitPeerReady(t, second)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	var proposal consoleapi.PendingQuestion
	for deadline := time.Now().Add(45 * time.Second); time.Now().Before(deadline); {
		status, body = PeerRequest(t, second, http.MethodGet, "/console/questions?conversation="+conversation, nil)
		if status == http.StatusOK {
			var response struct {
				Questions []consoleapi.PendingQuestion `json:"questions"`
			}
			if err := json.Unmarshal(body, &response); err != nil {
				t.Fatal(err)
			}
			for _, q := range response.Questions {
				if q.State == "pending" && q.Kind == "recovery" {
					for _, option := range q.Options {
						if strings.HasPrefix(option.ID, "confirm-stopped-and-retry:") {
							proposal = q
						}
					}
				}
			}
		}
		if proposal.ID != "" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if proposal.ID == "" {
		t.Fatalf("no concrete relocation proposal: %s", body)
	}
	if !strings.Contains(proposal.Message, oldRecord.Base) || !strings.Contains(proposal.Message, "可能需要重做") {
		t.Fatalf("proposal hides checkpoint or unsaved range: %+v", proposal)
	}
	approval := ""
	for _, option := range proposal.Options {
		if strings.HasPrefix(option.ID, "confirm-stopped-and-retry:") {
			approval = option.ID
		}
	}
	status, body = PeerRequest(t, second, http.MethodPost, "/console/questions/"+proposal.ID+"/answer", consoleapi.QuestionAnswer{CommandID: "accept-specific-relocation", Decision: "accept", Choice: approval})
	if status != http.StatusOK {
		t.Fatalf("approve plan: %d %s", status, body)
	}
	var question consoleapi.PendingQuestion
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		status, body = PeerRequest(t, second, http.MethodGet, "/console/questions?conversation="+conversation, nil)
		if status == http.StatusOK {
			var response struct {
				Questions []consoleapi.PendingQuestion `json:"questions"`
			}
			_ = json.Unmarshal(body, &response)
			for _, q := range response.Questions {
				if q.State == "pending" && q.AttemptID != original.AttemptID && q.Kind != "recovery" {
					question = q
				}
			}
		}
		if question.ID != "" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if question.ID == "" {
		t.Fatalf("replacement did not continue original goal: %s", body)
	}
	if question.TaskID != original.TaskID || question.ExchangeID != original.ExchangeID || question.SessionID == original.SessionID {
		t.Fatalf("replacement lost stable task/exchange or reused old native session: %+v", question)
	}
	status, body = PeerRequest(t, second, http.MethodPost, "/console/questions/"+question.ID+"/answer", consoleapi.QuestionAnswer{CommandID: "answer-replacement", Decision: "accept", Choice: "Blue"})
	if status != http.StatusOK {
		t.Fatalf("answer replacement: %d %s", status, body)
	}
	var finished consoleapi.Exchange
	for deadline := time.Now().Add(25 * time.Second); time.Now().Before(deadline); {
		status, body = PeerRequest(t, second, http.MethodGet, "/console/queue?conversation="+conversation, nil)
		if status == http.StatusOK {
			var list struct {
				Queue []consoleapi.Exchange `json:"queue"`
			}
			_ = json.Unmarshal(body, &list)
			for _, e := range list.Queue {
				if e.ID == original.ExchangeID && (e.State == "done" || e.State == "failed") {
					finished = e
				}
			}
		}
		if finished.ID != "" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if finished.State != "done" {
		t.Fatalf("replacement failed completion: %+v %s", finished, body)
	}
	book := WaitPeerReady(t, second).Ledger
	records, err := attempt.New(book).ForTask(t.Context(), original.TaskID)
	if err != nil || len(records) != 2 {
		t.Fatalf("replacement attempt count: %+v %v", records, err)
	}
	newRecord, err := attempt.New(book).Get(t.Context(), question.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if newRecord.PluginRuntime == nil || newRecord.PluginRuntimeID() == oldRecord.PluginRuntimeID() || !strings.Contains(string(newRecord.Result.Output), "PLUGIN_SKILL_github_1.0.0") || !strings.Contains(string(newRecord.Result.Output), "REVIEW_EVIDENCE/team-tools/1.0.0") {
		t.Fatalf("relocation lost original plugin version: %+v", newRecord)
	}
	if newRecord.State != attempt.Bound || newRecord.Workspace.Kind != "worktree" || newRecord.Node == oldRecord.Node || newRecord.Workspace.Path == oldRecord.Workspace.Path || newRecord.Recovery == nil {
		t.Fatalf("unsafe replacement workspace: %+v", newRecord)
	}
	tasks, err := task.OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	tracked, _ := tasks.Get(original.TaskID)
	if tracked.RecoveryWorkspace == nil || tracked.RecoveryWorkspace.NodeID != second.Config.NodeID || tracked.Budget.Turns != 1 {
		t.Fatalf("recovery workspace/turn continuity missing: %+v", tracked)
	}
	status, body = PeerRequest(t, second, http.MethodPost, "/console/send", consoleapi.Submission{Conversation: conversation, Input: "continue with next local check", CommandID: "follow-recovered-workspace"})
	if status != http.StatusOK {
		t.Fatalf("next turn did not use recovered workspace: %d %s", status, body)
	}
	var reply struct {
		Reply consoleapi.Reply `json:"reply"`
	}
	if json.Unmarshal(body, &reply) != nil || reply.Reply.Error != "" || reply.Reply.Injected == nil || reply.Reply.Injected.Node != second.Config.NodeID || reply.Reply.Injected.Workspace != newRecord.Workspace.Path {
		t.Fatalf("next turn went back to offline home: %s", body)
	}
}
