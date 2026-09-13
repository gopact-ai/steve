package app

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/cluster"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/hubid"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/readmodel"
	"github.com/gopact-ai/steve/internal/task"
)

func TestServiceBootstrapActivatesWithExistingTaskLedger(t *testing.T) {
	root := ClusterPeerTestDir(t)
	bin := filepath.Join(root, "mockagent")
	if output, err := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent").CombinedOutput(); err != nil {
		t.Fatalf("build ACP fixture: %v %s", err, output)
	}
	state := filepath.Join(root, "state")
	home := filepath.Join(root, "project")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(root, "service.json")
	raw, err := json.Marshal(map[string]any{
		"gateway":   map[string]any{"hub_id": "original-service", "owner_id": "original-owner", "state_path": filepath.Join(state, "state.json"), "read_model_addr": "127.0.0.1:0", "read_model_token": strings.Repeat("token", 8)},
		"projects":  map[string]any{"workspace": map[string]any{"home": map[string]any{"path": home}}},
		"agents":    map[string]any{"worker": map[string]any{"harness": "mock", "default": true}},
		"harnesses": map[string]any{"mock": map[string]any{"command": bin, "permission": "read"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := hubid.Resolve(state, "original-service"); err != nil {
		t.Fatal(err)
	}
	book, err := ledger.Open(state, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := task.OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	saved, err := tasks.Create(task.Task{Goal: "existing work", Channel: "console:existing", ProjectID: "workspace", Member: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Advance(saved.ID, task.StateDone); err != nil {
		t.Fatal(err)
	}
	if err := book.Close(); err != nil {
		t.Fatal(err)
	}
	sidecar, err := cluster.PrepareServiceCluster(cluster.ServiceBootstrap{ConfigPath: cfg, StorageLevel: "restricted"})
	if err != nil {
		t.Fatal(err)
	}
	peer := StartTestPeer(t, cluster.PeerOptions{ConfigPath: cfg, ClusterPath: sidecar})
	WaitPeerReady(t, peer)
	status, body := PeerRequest(t, peer, http.MethodGet, "/state", nil)
	if status != http.StatusOK {
		t.Fatalf("service did not activate: %d %s", status, body)
	}
	var snap readmodel.Snapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Tasks) != 1 || snap.Tasks[0].ID != saved.ID || snap.Tasks[0].Goal != "existing work" || snap.Tasks[0].State != task.StateDone {
		t.Fatalf("existing task lost: %+v", snap.Tasks)
	}
	if peer.Config.ClusterID != "original-service" {
		t.Fatal("service identity changed")
	}
	after, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(raw) {
		t.Fatal("cluster activation overwrote service configuration")
	}
	assertServiceWorkerTurn(t, peer)
}

func assertServiceWorkerTurn(t *testing.T, peer *cluster.Peer) {
	t.Helper()
	for _, input := range []string{"/project use workspace", "hello inherited worker"} {
		status, body := PeerRequest(t, peer, http.MethodPost, "/console/send", consoleapi.Submission{Conversation: "console:service-inherited-worker", Input: input, CommandID: input})
		var response struct {
			Reply consoleapi.Reply `json:"reply"`
		}
		if err := json.Unmarshal(body, &response); err != nil || status != http.StatusOK || response.Reply.Error != "" {
			t.Fatalf("inherited worker could not execute: %d %s %v", status, body, err)
		}
		if input == "hello inherited worker" && !strings.Contains(response.Reply.Text, input) {
			t.Fatalf("inherited worker did not answer: %s", body)
		}
	}
}
