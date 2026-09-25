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

	adminsvc "github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/cluster"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/task"
)

// Real peer, HTTP settings endpoint, durable tasks and native mock session;
// all data and processes are isolated. No external model or channel is used.
func TestApplicationLiveSettingsPreserveNativeSessionAndExistingBudget(t *testing.T) {
	dir := ClusterPeerTestDir(t)
	bin := filepath.Join(dir, "mockagent")
	if out, err := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent").CombinedOutput(); err != nil {
		t.Fatalf("build isolated agent: %v %s", err, out)
	}
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	if err := os.Mkdir(filepath.Join(dir, "memory"), 0o700); err != nil {
		t.Fatal(err)
	}
	options, installed := testPeerOptions(t, filepath.Join(dir, "app"), nil)
	ready := make(chan *adminsvc.Service, 1)
	options.ApplicationReady = func(a *adminsvc.Service, _ cluster.ApplicationServer, _ cluster.Activation) error {
		ready <- a
		return nil
	}
	cfg, err := cluster.LoadClusterPeerConfig(options.ClusterPath)
	if err != nil {
		t.Fatal(err)
	}
	token := cluster.ClusterRandomToken()
	worker := node.ServerConfig{Name: cfg.NodeID, Listen: "127.0.0.1:0", Token: token,
		Hubs: map[string]string{cfg.ClusterID: token}, StateDir: filepath.Join(cfg.DataDir, "node"),
		WorkspaceRoot: installed.Paths.Root, Harnesses: map[string]node.HarnessSpec{"mock": {
			Command: bin, Env: []string{"MOCKAGENT_MEMORY_DIR=" + filepath.Join(dir, "memory")},
		}}}
	if err := cluster.SaveClusterJSON(cfg.WorkerConfigFile, worker, true); err != nil {
		t.Fatal(err)
	}
	peer := StartTestPeer(t, options)
	generation := WaitPeerReady(t, peer).Generation
	a := <-ready
	continuityRequest(t, peer, http.MethodPost, "/console/agents", consoleapi.AddAgentRequest{ID: "worker", Harness: "mock", Node: cfg.NodeID})
	const conversation = "console:live-settings"
	continuitySend(t, peer, conversation, "/project use workspace", "bind-project")
	continuityRequest(t, peer, http.MethodPut, "/console/conversations/"+conversation, map[string]string{"title": "Live policy fixture"})
	first := continuitySend(t, peer, conversation, "fixture-remember hot-policy-memory", "remember")
	if first.Error != "" || !strings.Contains(first.Text, "memory: stored") {
		t.Fatalf("first turn failed: %+v", first)
	}
	session := continuityConversation(t, peer, conversation).Sessions["worker"]
	old, err := a.Tasks.Create(task.Task{Goal: "original budget"})
	if err != nil {
		t.Fatal(err)
	}
	read := func() consoleapi.SettingsView {
		t.Helper()
		status, body := PeerRequest(t, peer, http.MethodGet, "/console/settings", nil)
		var v consoleapi.SettingsView
		if status != http.StatusOK || json.Unmarshal(body, &v) != nil {
			t.Fatalf("settings read: %d", status)
		}
		return v
	}
	update := func(raw string) {
		t.Helper()
		status, body := PeerRequest(t, peer, http.MethodPut, "/console/settings", consoleapi.SettingsUpdate{BaseRevision: read().Revision, Settings: json.RawMessage(raw)})
		var v consoleapi.SettingsView
		if status != http.StatusOK || json.Unmarshal(body, &v) != nil || v.PendingRestart {
			t.Fatalf("live save failed or requested restart: %d %s", status, body)
		}
	}
	update(`{"gateway":{"locale":"en","prompt_timeout":"19m","task_max_turns":17,"task_max_elapsed":"3h"},"policies":{"landing":{"conflicts":"manual"},"snapshot":{"max_files":321},"review":{"max_entries":123}}}`)
	current, _ := a.Tasks.Get(old.ID)
	if current.Budget != old.Budget {
		t.Fatal("saving defaults rewrote existing task budget")
	}
	next, err := a.Tasks.Create(task.Task{Goal: "new budget"})
	if err != nil || next.Budget.MaxTurns != 17 || next.Budget.MaxElapsed != 3*time.Hour {
		t.Fatalf("new task did not consume saved budget: %+v %v", next.Budget, err)
	}
	limits, review := a.Artifacts.Policy()
	if limits.MaxFiles != 321 || review.MaxEntries != 123 {
		t.Fatal("artifact settings did not reach runtime consumer")
	}
	snap, err := a.HomeLoader.Load(home.ModeGuest)
	if err != nil || !strings.Contains(snap.Identity, "Steve") {
		t.Fatalf("live home loader failed: %v", err)
	}
	selected, ok := a.Catalog.Resolve("worker")
	if !ok {
		t.Fatal("worker missing")
	}
	caps, err := a.Assembler.AssembleMode(selected, home.ModeGuest)
	if err != nil || !strings.Contains(caps.Instructions, home.LanguageRule(home.LocaleEN)) {
		t.Fatalf("saved language missing from next capability assembly: %v", err)
	}
	reply := continuitySend(t, peer, conversation, "fixture-recall", "recall")
	after := continuityConversation(t, peer, conversation).Sessions["worker"]
	if reply.Error != "" || !strings.Contains(reply.Text, "memory: hot-policy-memory") || after.UpstreamID != session.UpstreamID || after.AgentToken != session.AgentToken {
		t.Fatalf("policy update broke native continuation: %s %s", reply.Error, reply.Text)
	}
	update(`{"gateway":{"task_max_turns":0,"task_max_elapsed":"0s"}}`)
	unlimited, err := a.Tasks.Create(task.Task{Goal: "unlimited restored"})
	if err != nil || unlimited.Budget.MaxTurns != 0 || unlimited.Budget.MaxElapsed != 0 {
		t.Fatalf("zero defaults not restored: %+v %v", unlimited.Budget, err)
	}
	if WaitPeerReady(t, peer).Generation != generation {
		t.Fatal("saving runtime policy restarted the application")
	}
	// A saved prompt timeout reaches the running coordinator: a silent turn
	// ends on a saved 3s, which leaves a turn prepared under -race room to
	// reach its prompt. A coordinator that kept its startup timeout would
	// leave the turn running past the bound.
	const expiring = "console:live-timeout"
	continuitySend(t, peer, expiring, "/project use workspace", "bind-timeout")
	const timeout = 3 * time.Second
	update(`{"gateway":{"prompt_timeout":"` + timeout.String() + `"}}`)
	const bound = 20 * time.Second
	started := time.Now()
	status, body, err := peerRequestWithin(t, peer, bound, http.MethodPost, "/console/send", consoleapi.Submission{
		Conversation: expiring, Input: "slow expired-turn", CommandID: "expired-turn",
	})
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("a silent turn under a saved %v prompt timeout did not end within %v: %v", timeout, bound, err)
	}
	var failed struct {
		Error string `json:"error"`
	}
	if status != http.StatusBadGateway || json.Unmarshal(body, &failed) != nil ||
		!strings.Contains(failed.Error, context.DeadlineExceeded.Error()) || elapsed < timeout {
		t.Fatalf("silent turn under a saved %v prompt timeout: %d after %v: %s", timeout, status, elapsed, body)
	}
	if WaitPeerReady(t, peer).Generation != generation {
		t.Fatal("ending a turn on its prompt timeout restarted the application")
	}
}
