package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gopact-ai/steve/internal/cluster"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/plugins"
)

type peerPluginFixture struct {
	sources map[string]string
	targets map[string]plugins.Configuration
	service *httptest.Server
	calls   atomic.Int32
}

func preparePeerPluginFixture(t *testing.T, nodes ...*cluster.Peer) *peerPluginFixture {
	t.Helper()
	f := &peerPluginFixture{sources: map[string]string{}, targets: map[string]plugins.Configuration{}}
	f.service = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer node-private-plugin-test" {
			http.Error(w, "wrong node credential", http.StatusUnauthorized)
			return
		}
		var call struct {
			Method string `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&call); err != nil {
			http.Error(w, "bad RPC", http.StatusBadRequest)
			return
		}
		result := map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "isolated-plugin", "version": "1"}}
		if call.Method == "tools/call" {
			f.calls.Add(1)
			result = map[string]any{"content": []map[string]string{{"type": "text", "text": "REVIEW_EVIDENCE" + r.URL.Path}}}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
	}))
	t.Cleanup(f.service.Close)
	for _, peer := range nodes {
		store := &plugins.Store{Dir: filepath.Join(peer.Config.DataDir, "node", "plugins")}
		secret, err := store.PutSecret(t.Context(), "package-test", "node-private-plugin-test")
		if err != nil {
			t.Fatal(err)
		}
		f.targets[peer.Config.NodeID] = plugins.Configuration{Secrets: map[string]plugins.SecretRef{"token": secret.Reference}}
	}
	for _, example := range []string{"github", "team-tools"} {
		source := t.TempDir()
		if err := os.CopyFS(source, os.DirFS(filepath.Join("..", "..", "examples", "plugins", example))); err != nil {
			t.Fatal(err)
		}
		f.sources[example] = source
	}
	return f
}

func pluginPeerJSON(t *testing.T, peer *cluster.Peer, method, path string, request, response any) {
	t.Helper()
	status, body := PeerRequest(t, peer, method, path, request)
	if status != http.StatusOK {
		t.Fatalf("%s %s: %d %s", method, path, status, body)
	}
	if response != nil {
		if err := json.Unmarshal(body, response); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *peerPluginFixture) activate(t *testing.T, peer *cluster.Peer, version string) {
	t.Helper()
	for _, name := range []string{"github", "team-tools"} {
		source := f.sources[name]
		raw, err := os.ReadFile(filepath.Join(source, "plugin.json"))
		if err != nil {
			t.Fatal(err)
		}
		var manifest plugins.Manifest
		if err := json.Unmarshal(raw, &manifest); err != nil {
			t.Fatal(err)
		}
		manifest.Version = version
		for id, preset := range manifest.Agents {
			preset.Harness = "mock"
			manifest.Agents[id] = preset
		}
		for id, server := range manifest.MCP {
			server.URL = plugins.Value{Text: f.service.URL + "/" + name + "/" + version}
			manifest.MCP[id] = server
		}
		for _, skill := range manifest.Skills {
			if err := os.WriteFile(filepath.Join(source, skill, "SKILL.md"), []byte("PLUGIN_SKILL_"+name+"_"+version), 0600); err != nil {
				t.Fatal(err)
			}
		}
		data, _ := json.Marshal(manifest)
		if err := os.WriteFile(filepath.Join(source, "plugin.json"), data, 0600); err != nil {
			t.Fatal(err)
		}
		var preview consoleapi.PluginPreview
		pluginPeerJSON(t, peer, http.MethodPost, "/console/plugins/preview", plugins.Source{Kind: "directory", Location: source}, &preview)
		pluginPeerJSON(t, peer, http.MethodPost, "/console/plugins/import", consoleapi.PluginImportRequest{CommandID: "import-" + name + "-" + version, Project: "workspace", Digest: preview.Digest, Source: preview.Source}, nil)
		var view consoleapi.PluginsView
		pluginPeerJSON(t, peer, http.MethodGet, "/console/plugins", nil, &view)
		targets := map[string]plugins.Configuration{}
		for node, config := range f.targets {
			config = config.Clone()
			if name == "team-tools" {
				config.Values = map[string]string{"endpoint": f.service.URL}
			}
			targets[node] = config
		}
		pluginPeerJSON(t, peer, http.MethodPut, "/console/plugins/installations/"+name, consoleapi.PluginUpdateRequest{BaseRevision: view.Revision, Installation: plugins.Installation{PackageID: manifest.ID, Digest: preview.Digest, Enabled: true, Projects: []string{"workspace"}, Targets: targets}}, &view)
		serialized, _ := json.Marshal(view)
		if strings.Contains(string(serialized), "node-private-plugin-test") {
			t.Fatal("node credential reached shared metadata")
		}
	}
}

func adoptPeerPluginAgent(t *testing.T, peer *cluster.Peer, node string) {
	t.Helper()
	var view consoleapi.PluginsView
	pluginPeerJSON(t, peer, http.MethodGet, "/console/plugins", nil, &view)
	digest := ""
	for _, item := range view.Installations {
		if item.ID == "github" {
			digest = item.Installation.Digest
		}
	}
	req := consoleapi.PluginPresetRequest{CommandID: "adopt-worker", AgentID: "worker", Node: node, Preset: "reviewer", Digest: digest, Adopt: &plugins.Adoption{}}
	var preview consoleapi.PluginPresetPreview
	pluginPeerJSON(t, peer, http.MethodPost, "/console/plugins/installations/github/presets/preview", req, &preview)
	req.BaseRevision = preview.Revision
	pluginPeerJSON(t, peer, http.MethodPost, "/console/plugins/installations/github/presets/apply", req, nil)
}
