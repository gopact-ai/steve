package cluster

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/platformconfig"
)

// recordingApplication stands in for the local application: it remembers
// project home moves and answers ok.
func recordingApplication(t *testing.T, calls *[]string, mu *sync.Mutex) func(context.Context, Activation, func(PeerApplicationEndpoint) error) (Deactivate, error) {
	t.Helper()
	return func(ctx context.Context, activation Activation, ready func(PeerApplicationEndpoint) error) (Deactivate, error) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		token := ClusterRandomToken()
		server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !ConstantToken(r.Header.Get("Authorization"), token) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			body, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
			mu.Lock()
			*calls = append(*calls, r.Method+" "+r.URL.Path+" "+strings.TrimSpace(string(body)))
			mu.Unlock()
			WriteJSON(w, map[string]any{"ok": true})
		}), BaseContext: func(net.Listener) context.Context { return ctx }}
		go func() { _ = server.Serve(listener) }()
		if err := ready(PeerApplicationEndpoint{URL: "http://" + listener.Addr().String(), Token: token}); err != nil {
			server.Close()
			return nil, err
		}
		return func(context.Context) error { return server.Close() }, nil
	}
}

func TestClusterPeerDesktopGuideProgressAndWorkspace(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	options, installed := testPeerOptions(t, ClusterPeerTestDir(t), nil)
	var calls []string
	var mu sync.Mutex
	options.Activate = recordingApplication(t, &calls, &mu)
	peer := StartTestPeer(t, options)
	active := WaitPeerReady(t, peer)

	var status consoleapi.DesktopStatus
	code, body := PeerRequest(t, peer, http.MethodGet, "/console/desktop", nil)
	if err := json.Unmarshal(body, &status); err != nil || code != http.StatusOK {
		t.Fatalf("status: %d %s %v", code, body, err)
	}
	if !status.Enabled || !status.SetupRequired || status.Setup == nil || status.Setup.Step != "identity" || status.WorkspacePath != installed.Paths.Root || !status.WorkspaceManaged {
		t.Fatalf("a fresh desktop opens the guide and names its still-managed workspace: %+v", status)
	}

	code, body = PeerRequest(t, peer, http.MethodPut, "/console/desktop/setup", consoleapi.DesktopSetupRequest{Step: "machines"})
	if err := json.Unmarshal(body, &status); err != nil || code != http.StatusOK || status.Setup.Step != "machines" || !status.SetupRequired {
		t.Fatalf("progress: %d %s %v", code, body, err)
	}
	if code, body = PeerRequest(t, peer, http.MethodPut, "/console/desktop/setup", map[string]any{"step": "machines", "extra": true}); code != http.StatusBadRequest {
		t.Fatalf("unknown fields are refused: %d %s", code, body)
	}
	if code, body = PeerRequest(t, peer, http.MethodPut, "/console/desktop/setup", consoleapi.DesktopSetupRequest{Step: "nowhere"}); code != http.StatusBadRequest {
		t.Fatalf("unknown steps are refused: %d %s", code, body)
	}

	// Once the application has published the shared declaration, the local
	// project's home names this node rather than leaving the node empty.
	worker := peer.Worker()
	declared := platformconfig.Declaration{Settings: (&config.Config{Gateway: config.Gateway{OwnerID: "test-owner"}}).SettingsValues(), Channels: (&config.Config{Gateway: config.Gateway{OwnerID: "test-owner"}}).ChannelSettings(),
		Home: config.ProjectHome{Node: peer.Config.NodeID, Path: filepath.Join(installed.Paths.Root, "home")}, DefaultProject: "workspace",
		Nodes:    map[string]config.Node{worker.Name: {Addr: worker.Address, Token: worker.Token, Level: "restricted"}},
		Projects: map[string]config.Project{"workspace": {Level: "internal", Home: config.ProjectHome{Node: peer.Config.NodeID, Path: filepath.Join(installed.Paths.Root, "workspace")}}}}
	if _, err := platformconfig.New(active.Ledger).Save(t.Context(), 0, declared); err != nil {
		t.Fatal(err)
	}
	code, body = PeerRequest(t, peer, http.MethodGet, "/console/desktop", nil)
	if err := json.Unmarshal(body, &status); err != nil || code != http.StatusOK || status.WorkspacePath != installed.Paths.Root {
		t.Fatalf("status after the declaration: %d %s %v", code, body, err)
	}

	if code, body = PeerRequest(t, peer, http.MethodPut, "/console/desktop/workspace", consoleapi.DesktopWorkspaceRequest{Path: "/etc"}); code != http.StatusBadRequest || !strings.Contains(string(body), "系统目录") {
		t.Fatalf("system directories are refused: %d %s", code, body)
	}
	mu.Lock()
	if len(calls) != 0 {
		t.Fatalf("a refused directory reached the application: %v", calls)
	}
	mu.Unlock()
	code, body = PeerRequest(t, peer, http.MethodPut, "/console/desktop/workspace", consoleapi.DesktopWorkspaceRequest{Path: "~/Steve"})
	if code != http.StatusOK {
		t.Fatalf("workspace: %d %s", code, body)
	}
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, "Steve")
	if info, err := os.Stat(want); err != nil || !info.IsDir() {
		t.Fatalf("the directory is created before the project moves: %v", err)
	}
	if root := peer.worker.WorkspaceRoot(); root != want {
		t.Fatalf("the running execution service still works in %s", root)
	}
	if root := peer.WorkerWorkspaceRoot(); root != want {
		t.Fatalf("the application would be built for %s", root)
	}
	var stored struct {
		WorkspaceRoot string `json:"workspace_root"`
	}
	raw, err := os.ReadFile(peer.Config.WorkerConfigFile)
	if err != nil || json.Unmarshal(raw, &stored) != nil || stored.WorkspaceRoot != want {
		t.Fatalf("the choice was not written where a restart reads it: %q %v", stored.WorkspaceRoot, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 || calls[0] != `PUT /console/projects/workspace/home {"path":"workspace"}` {
		t.Fatalf("the default project moves in under the workspace, not onto it: %v", calls)
	}

	mu.Unlock()
	declared.Projects["workspace"] = config.Project{Level: "internal", Home: config.ProjectHome{Node: "gpu-box", Path: "/srv/steve"}}
	declared.Nodes["gpu-box"] = config.Node{Addr: "127.0.0.1:1", Token: "t", Level: "restricted"}
	if _, err := platformconfig.New(active.Ledger).Save(t.Context(), 1, declared); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(home, "Elsewhere")
	if code, body = PeerRequest(t, peer, http.MethodPut, "/console/desktop/workspace", consoleapi.DesktopWorkspaceRequest{Path: "~/Elsewhere"}); code != http.StatusOK {
		t.Fatalf("where this computer works is its own to choose: %d %s", code, body)
	}
	if root := peer.worker.WorkspaceRoot(); root != elsewhere {
		t.Fatalf("the new choice did not reach the execution service: %s", root)
	}
	mu.Lock()
	if len(calls) != 1 {
		t.Fatalf("a default project on another machine must not be moved: %v", calls)
	}

	code, body = PeerRequest(t, peer, http.MethodPut, "/console/desktop/setup", consoleapi.DesktopSetupRequest{Step: "finished", Done: true})
	if err := json.Unmarshal(body, &status); err != nil || code != http.StatusOK || status.SetupRequired || !status.Setup.Done {
		t.Fatalf("finishing: %d %s %v", code, body, err)
	}
	if err := peer.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := StartTestPeer(t, options)
	WaitPeerReady(t, reopened)
	code, body = PeerRequest(t, reopened, http.MethodGet, "/console/desktop", nil)
	if err := json.Unmarshal(body, &status); err != nil || code != http.StatusOK || status.SetupRequired || !status.Setup.Done {
		t.Fatalf("progress survives a restart: %d %s %v", code, body, err)
	}
}
