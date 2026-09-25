package app

import (
	"context"
	"encoding/json"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/cluster"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/node"
)

// Real peer and node with the mock agent. A node-owned turn whose agent goes
// silent ends on the saved prompt timeout: the native command is stopped, the
// console send returns the timeout, and the conversation stays usable.
func TestNodeManagedSilentTurnEndsOnItsPromptTimeout(t *testing.T) {
	dir := ClusterPeerTestDir(t)
	bin := filepath.Join(dir, "mockagent")
	if out, err := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent").CombinedOutput(); err != nil {
		t.Fatalf("build isolated agent: %v %s", err, out)
	}
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	options, installed := testPeerOptions(t, filepath.Join(dir, "app"), nil)
	cfg, err := cluster.LoadClusterPeerConfig(options.ClusterPath)
	if err != nil {
		t.Fatal(err)
	}
	token := cluster.ClusterRandomToken()
	worker := node.ServerConfig{Name: cfg.NodeID, Listen: "127.0.0.1:0", Token: token,
		Hubs: map[string]string{cfg.ClusterID: token}, StateDir: filepath.Join(cfg.DataDir, "node"),
		WorkspaceRoot: installed.Paths.Root, Harnesses: map[string]node.HarnessSpec{"mock": {Command: bin}}}
	if err := cluster.SaveClusterJSON(cfg.WorkerConfigFile, worker, true); err != nil {
		t.Fatal(err)
	}
	peer := StartTestPeer(t, options)
	WaitPeerReady(t, peer)
	continuityRequest(t, peer, http.MethodPost, "/console/agents", consoleapi.AddAgentRequest{ID: "worker", Harness: "mock", Node: cfg.NodeID})
	const conversation = "console:silent-node-turn"
	continuitySend(t, peer, conversation, "/project use workspace", "bind-project")
	if warm := continuitySend(t, peer, conversation, "hello", "warm"); warm.Error != "" {
		t.Fatalf("warm-up turn failed: %+v", warm)
	}
	// Preparing a turn under -race can stay quiet for over a second; the
	// timeout leaves it room so the clock runs out on the silent prompt.
	const timeout = 3 * time.Second
	savePromptTimeout(t, peer, timeout.String())

	// Without a stop the silent command never ends; the bound turns a
	// regression into a failure instead of a hung test.
	const bound = 20 * time.Second
	started := time.Now()
	status, body, err := peerRequestWithin(t, peer, bound, http.MethodPost, "/console/send", consoleapi.Submission{
		Conversation: conversation, Input: "slow silent work", CommandID: "silent",
	})
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("a silent node-owned turn did not end on its %v prompt timeout within %v: %v", timeout, bound, err)
	}
	var failed struct {
		Error string `json:"error"`
	}
	if status != http.StatusBadGateway || json.Unmarshal(body, &failed) != nil || !strings.Contains(failed.Error, context.DeadlineExceeded.Error()) {
		t.Fatalf("silent turn after %v: %d %s", elapsed, status, body)
	}
	if elapsed < timeout || !strings.Contains(failed.Error, harness.ErrTurnCanceled.Error()) {
		t.Fatalf("silent turn ended after %v without stopping its native command: %s", elapsed, failed.Error)
	}
	// The follow-up runs under a roomy timeout: it checks the conversation,
	// not how fast a loaded run prepares a turn.
	savePromptTimeout(t, peer, "10m")
	if next := continuitySend(t, peer, conversation, "hello again", "after-timeout"); next.Error != "" {
		t.Fatalf("conversation after the timeout: %+v", next)
	}
}

func savePromptTimeout(t *testing.T, peer *cluster.Peer, timeout string) {
	t.Helper()
	var current consoleapi.SettingsView
	if status, body := PeerRequest(t, peer, http.MethodGet, "/console/settings", nil); status != http.StatusOK || json.Unmarshal(body, &current) != nil {
		t.Fatalf("settings read: %d %s", status, body)
	}
	update := consoleapi.SettingsUpdate{BaseRevision: current.Revision, Settings: json.RawMessage(`{"gateway":{"prompt_timeout":"` + timeout + `"}}`)}
	var saved consoleapi.SettingsView
	if status, body := PeerRequest(t, peer, http.MethodPut, "/console/settings", update); status != http.StatusOK || json.Unmarshal(body, &saved) != nil || saved.PendingRestart {
		t.Fatalf("live save failed or requested restart: %d %s", status, body)
	}
}
