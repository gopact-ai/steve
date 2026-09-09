package node

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/acp"

	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/plugins"
)

func nodeRuntimeFixture(t *testing.T, s *Server, endpoint string) plugins.Selection {
	t.Helper()
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "skill"), 0700)
	manifest := plugins.Manifest{Schema: plugins.Schema, API: plugins.API, ID: "test/runtime", Version: "1.0.0", Description: "runtime fixture", Skills: map[string]string{"skill": "skill"}}
	if endpoint != "" {
		manifest.Settings = map[string]plugins.Setting{"token": {Description: "node token", Secret: true, Required: true}}
		manifest.MCP = map[string]plugins.MCPServer{"api": {Transport: "http", URL: plugins.Value{Text: endpoint}, Headers: map[string]plugins.Value{"Authorization": {Secret: "token", Prefix: "Bearer "}}}}
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "plugin.json"), raw, 0600)
	os.WriteFile(filepath.Join(dir, "skill/SKILL.md"), []byte("PLUGIN_RUNTIME_ORIGINAL"), 0600)
	bundle, err := plugins.ReadDirectory(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	store := s.pluginStore()
	if _, err := store.Install(t.Context(), plugins.InstallRequest{CommandID: "package", ExpectedDigest: bundle.Digest, Bundle: bundle, Source: plugins.Source{Kind: "bundle", Location: bundle.Digest}}); err != nil {
		t.Fatal(err)
	}
	cfg := plugins.Configuration{}
	if endpoint != "" {
		secret, err := store.PutSecret(t.Context(), "token", "LOCAL_SECRET")
		if err != nil {
			t.Fatal(err)
		}
		cfg.Secrets = map[string]plugins.SecretRef{"token": secret.Reference}
	}
	deployment := plugins.Deployment{Installation: "runtime", PackageID: manifest.ID, Digest: bundle.Digest, Node: s.conf().Name, Projects: []string{"p"}, Configuration: cfg}
	receipt, err := store.PrepareDeployment(t.Context(), deployment, plugins.Environment{})
	if err != nil {
		t.Fatal(err)
	}
	return plugins.Selection{Project: "p", Node: s.conf().Name, Harness: "mock", Deployments: []string{receipt.Hash}}
}

func TestPluginBrokerRetainsRouteAndSecretVersionAcrossRestart(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer LOCAL_SECRET" {
			t.Error("wrong node credential")
		}
		w.Write([]byte("original service"))
	}))
	defer upstream.Close()
	s := NewServer(ServerConfig{Name: "worker", StateDir: t.TempDir()})
	selection := nodeRuntimeFixture(t, s, upstream.URL)
	pool := s.pluginRuntimePool()
	runtime, err := pool.Prepare(t.Context(), "runtime", selection, harness.Config{Command: "test"})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err = pool.Load(t.Context(), runtime.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(runtime.Servers) != 1 || strings.Contains(runtime.Servers[0].URL, "LOCAL_SECRET") || len(runtime.Servers[0].Headers) != 0 {
		t.Fatal("upstream credentials leaked")
	}
	url := runtime.Servers[0].URL
	check := func() {
		t.Helper()
		response, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		raw, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != 200 || string(raw) != "original service" {
			t.Fatalf("proxy: %s %v", raw, err)
		}
	}
	check()
	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}
	replacement := &PluginRuntimePool{Store: s.pluginStore(), StateDir: s.conf().StateDir}
	defer replacement.Close()
	restored, err := replacement.Load(t.Context(), runtime.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Servers[0].URL != url {
		t.Fatal("existing session MCP route changed")
	}
	check()
}

func TestNodeOwnedPluginSessionPersistsRuntimeBinding(t *testing.T) {
	authority := &sessionAuthorityTest{epoch: 1, writer: 1}
	bin := buildMockAgent(t)
	s := NewServer(ServerConfig{Name: "worker", StateDir: t.TempDir(), Harnesses: map[string]HarnessSpec{"mock": {Command: bin}}, SessionAuthorizer: authority})
	selection := nodeRuntimeFixture(t, s, "")
	runtime, err := s.pluginRuntimePool().Prepare(t.Context(), "native", selection, harness.Config{Command: bin, Permission: "read"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := s.startSessions(ctx); err != nil {
		t.Fatal(err)
	}
	defer s.sessions.Close()
	defer s.closePluginRuntimes()
	req := nodeSessionRequest(nodewire.SessionActionOpen)
	req.Harness = "mock"
	req.Workdir = t.TempDir()
	req.CommandID = "open-1"
	req.Plugin = &runtime.Ref
	req.Binding.PluginRuntimeID = runtime.Ref.ID
	state, err := s.sessions.Do(ctx, "cluster-1", req)
	if err != nil {
		t.Fatal(err)
	}
	if state.Plugin == nil || state.Plugin.ID != runtime.Ref.ID {
		t.Fatal("runtime binding not persisted")
	}
	req.ID = state.ID
	req.Action = nodewire.SessionActionPrompt
	req.CommandID = "input-1"
	req.InputSequence = 1
	req.Text = "hello"
	if _, err := s.sessions.Do(ctx, "cluster-1", req); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		req.Action = nodewire.SessionActionPoll
		req.After = state.Sequence
		req.WaitMS = 100
		state, err = s.sessions.Do(ctx, "cluster-1", req)
		if err != nil {
			t.Fatal(err)
		}
		if state.Command != nil && state.Command.Settled {
			break
		}
	}
	if state.Command == nil || !state.Command.Settled || !strings.Contains(state.Command.Output, "PLUGIN_RUNTIME_ORIGINAL") {
		t.Fatalf("plugin instruction absent: %+v", state.Command)
	}
	authority.mu.Lock()
	authority.epoch = 2
	authority.writer = 2
	authority.mu.Unlock()
	req.Authority.CoordinatorEpoch = 2
	req.Authority.WriterGeneration = 2
	req.Authority.CoordinatorNodeID = "hub-b"
	req.Action = nodewire.SessionActionAttach
	attached, err := s.sessions.Do(ctx, "cluster-1", req)
	if err != nil || attached.Plugin == nil || attached.Plugin.ID != runtime.Ref.ID || attached.InputAccepted != 1 {
		t.Fatalf("coordinator replacement lost original runtime: %+v %v", attached, err)
	}
	wrong := req
	wrong.Binding.PluginRuntimeID = strings.Repeat("0", 64)
	if _, err := s.sessions.Do(ctx, "cluster-1", wrong); err == nil {
		t.Fatal("reattachment accepted a different runtime")
	}

	req.Action = nodewire.SessionActionClose
	if _, err := s.sessions.Do(ctx, "cluster-1", req); err != nil {
		t.Fatal(err)
	}
}

type nodePluginProvider struct{ registry *Registry }

func (p nodePluginProvider) PluginRuntime(ctx context.Context, at harness.Placement, ref plugins.RuntimeRef) (harness.Config, []acp.MCPServer, error) {
	reply, err := p.registry.Plugins(ctx, at.Node, nodewire.PluginRequest{Action: nodewire.PluginRuntimeInspect, Selection: &ref.Selection, Runtime: &ref})
	return harness.Config{Permission: "read", PluginInstructions: reply.Instructions}, reply.Servers, err
}

func TestRemotePluginProcessUsesOriginalRuntimeAndRejectsOtherRuntime(t *testing.T) {
	bin := buildMockAgent(t)
	s := startNode(t, ServerConfig{Name: "worker", Token: "plugin", StateDir: t.TempDir(), Harnesses: map[string]HarnessSpec{"mock": {Command: bin}}})
	registry := NewRegistry("owner", map[string]Config{"worker": {Addr: s.Addr(), Token: "plugin"}})
	t.Cleanup(registry.Close)
	selection := nodeRuntimeFixture(t, s, "")
	prepared, err := registry.Plugins(t.Context(), "worker", nodewire.PluginRequest{Action: nodewire.PluginRuntimePrepare, Selection: &selection, CommandID: "remote"})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := harness.NewManager(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Stop)
	manager.SetTransports(registry)
	manager.SetPluginRuntimes(nodePluginProvider{registry})
	at := harness.Placement{Node: "worker", Harness: "mock"}
	work := t.TempDir()
	session, err := manager.OpenSession(harness.WithPluginProfile(t.Context(), prepared.Runtime), at, "", work, nil)
	if err != nil {
		t.Fatal(err)
	}
	answer, _, err := session.Prompt(t.Context(), "hello", nil)
	if err != nil || !strings.Contains(answer, "PLUGIN_RUNTIME_ORIGINAL") {
		t.Fatalf("remote runtime: %q %v", answer, err)
	}
	other, err := registry.Plugins(t.Context(), "worker", nodewire.PluginRequest{Action: nodewire.PluginRuntimePrepare, Selection: &selection, CommandID: "different"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.OpenSession(harness.WithPluginProfile(t.Context(), other.Runtime), at, session.ID(), work, nil); err == nil {
		t.Fatal("another runtime resumed original native session")
	}
	resumed, err := manager.OpenSession(harness.WithPluginProfile(t.Context(), prepared.Runtime), at, session.ID(), work, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.ID() != session.ID() {
		t.Fatal("runtime reattachment changed public session identity")
	}
	if err := manager.CloseSession(t.Context(), at, session.ID()); err != nil {
		t.Fatal(err)
	}
}
