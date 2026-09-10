package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/cluster"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/plugins"
)

func TestPeerInitCommandRejectsArgumentsBeforeWriting(t *testing.T) {
	for _, args := range [][]string{nil, {"--state-dir", " "}, {"--config", "existing.json"}, {"--state-dir", filepath.Join(t.TempDir(), "new"), "extra"}} {
		var out, diagnostic bytes.Buffer
		if err := runPeerInitCommand(args, &out, &diagnostic); err == nil || out.Len() != 0 {
			t.Fatalf("invalid arguments accepted: %v", args)
		}
		if len(args) == 3 {
			if _, err := os.Stat(args[1]); !os.IsNotExist(err) {
				t.Fatal("invalid arguments created state")
			}
		}
	}
}

func TestServicePeerInitializationDeploysPluginThroughCoordinator(t *testing.T) {
	root := ClusterPeerTestDir(t)
	var out, diagnostic bytes.Buffer
	if err := runPeerInitCommand([]string{"--state-dir", root}, &out, &diagnostic); err != nil {
		t.Fatal(err)
	}
	var initialized cluster.PeerInitialization
	if err := json.Unmarshal(out.Bytes(), &initialized); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	workerState := t.TempDir()
	const workerToken = "isolated-service-worker-credential"
	// Match the released steve-node's authorizer; no permissive test stub.
	worker := node.NewServer(node.ServerConfig{Name: "worker", Listener: listener, Listen: listener.Addr().String(),
		Token: workerToken, Hubs: map[string]string{initialized.ClusterID: workerToken}, StateDir: workerState,
		WorkspaceRoot: t.TempDir(), SessionAuthorizer: node.CoordinatorSessionAuthorizer{}})
	done := make(chan error, 1)
	go func() { done <- worker.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("worker shutdown: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("worker shutdown timed out")
		}
	})
	cfg, err := config.Load(initialized.Config)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Nodes = map[string]config.Node{"worker": {Addr: listener.Addr().String(), Token: workerToken, Level: "restricted"}}
	if err := config.Save(initialized.Config, cfg); err != nil {
		t.Fatal(err)
	}
	peer := StartTestPeer(t, cluster.PeerOptions{ConfigPath: initialized.Config, ClusterPath: initialized.ClusterConfig})
	active := WaitPeerReady(t, peer)
	if active.Assignment.Epoch == 0 || active.WriterGeneration == 0 {
		t.Fatal("service has no committed coordinator authority")
	}
	source := t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "review"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "review", "SKILL.md"), []byte("Review the project."), 0600); err != nil {
		t.Fatal(err)
	}
	manifest := plugins.Manifest{Schema: plugins.Schema, API: plugins.API, ID: "example/service", Version: "1.0.0", Description: "Service deployment fixture", Skills: map[string]string{"review": "review"}}
	raw, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(source, "plugin.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	var preview consoleapi.PluginPreview
	pluginPeerJSON(t, peer, http.MethodPost, "/console/plugins/preview", plugins.Source{Kind: "directory", Location: source}, &preview)
	pluginPeerJSON(t, peer, http.MethodPost, "/console/plugins/import", consoleapi.PluginImportRequest{CommandID: "service-import", Project: "workspace", Digest: preview.Digest, Source: preview.Source}, nil)
	var view consoleapi.PluginsView
	pluginPeerJSON(t, peer, http.MethodGet, "/console/plugins", nil, &view)
	installation := plugins.Installation{PackageID: manifest.ID, Digest: preview.Digest, Enabled: true, Projects: []string{"workspace"}, Targets: map[string]plugins.Configuration{"worker": {}}}
	pluginPeerJSON(t, peer, http.MethodPut, "/console/plugins/installations/service", consoleapi.PluginUpdateRequest{BaseRevision: view.Revision, Installation: installation}, nil)
	var prepared consoleapi.PluginInstallationView
	pluginPeerJSON(t, peer, http.MethodPost, "/console/plugins/installations/service/prepare", struct{}{}, &prepared)
	if len(prepared.Targets) != 1 || prepared.Targets[0].State != plugins.Prepared || prepared.Targets[0].Receipt == nil {
		t.Fatalf("service could not deploy to released node authorization: %+v", prepared.Targets)
	}
	store := &plugins.Store{Dir: filepath.Join(workerState, "plugins")}
	if _, err := store.Read(preview.Digest); err != nil {
		t.Fatalf("package did not reach worker: %v", err)
	}
	address := peer.UiURL
	if err := peer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := cluster.InitializePeer(root); err != nil {
		t.Fatalf("retry after first run: %v", err)
	}
	restarted := StartTestPeer(t, cluster.PeerOptions{ConfigPath: initialized.Config, ClusterPath: initialized.ClusterConfig})
	WaitPeerReady(t, restarted)
	if restarted.UiURL != address || restarted.Config.ClusterID != initialized.ClusterID {
		t.Fatal("restart changed service origin or cluster identity")
	}
	pluginPeerJSON(t, restarted, http.MethodPost, "/console/plugins/installations/service/prepare", struct{}{}, &prepared)
	if len(prepared.Targets) != 1 || prepared.Targets[0].State != plugins.Prepared {
		t.Fatalf("restart lost plugin authority: %+v", prepared.Targets)
	}
}
