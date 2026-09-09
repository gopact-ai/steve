package node

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/plugins"
)

func pluginFixture(t *testing.T, server *Server) (plugins.Bundle, plugins.Deployment) {
	t.Helper()
	bundle, err := plugins.ReadDirectory(t.Context(), filepath.Join("..", "..", "examples/plugins/team-tools"))
	if err != nil {
		t.Fatal(err)
	}
	ref, err := server.pluginStore().PutSecret(t.Context(), "team-token", "secret-stays-on-worker")
	if err != nil {
		t.Fatal(err)
	}
	d := plugins.Deployment{Installation: "team", PackageID: bundle.Manifest.ID, Digest: bundle.Digest, Node: server.conf().Name, Projects: []string{"work"}, Configuration: plugins.Configuration{Values: map[string]string{"endpoint": "https://example.invalid/mcp"}, Secrets: map[string]plugins.SecretRef{"token": ref.Reference}}}
	return bundle, d
}

func TestPluginPrepareUsesAuthenticatedNodeAndKeepsSecretsLocal(t *testing.T) {
	server := startNode(t, ServerConfig{Name: "worker", Token: "plugin-test", StateDir: t.TempDir()})
	registry := NewRegistry("owner", map[string]Config{"worker": {Addr: server.Addr(), Token: "plugin-test"}})
	t.Cleanup(registry.Close)
	bundle, d := pluginFixture(t, server)
	request := nodewire.PluginRequest{Action: nodewire.PluginPrepare, Deployment: d, Bundle: bundle.Data}
	reply, err := registry.Plugins(t.Context(), "worker", request)
	if err != nil {
		t.Fatal(err)
	}
	wanted, _ := d.Hash()
	if reply.Receipt == nil || reply.Receipt.Hash != wanted {
		t.Fatal("wrong deployment receipt")
	}
	raw, err := json.Marshal(reply)
	if err != nil || strings.Contains(string(raw), "secret-stays-on-worker") {
		t.Fatal("secret crossed control plane")
	}
	request.Action = nodewire.PluginInspect
	request.Bundle = nil
	again, err := registry.Plugins(t.Context(), "worker", request)
	if err != nil || again.Receipt.Hash != wanted {
		t.Fatalf("inspect: %v", err)
	}
	request.Action = nodewire.PluginSecrets
	metadata, err := registry.Plugins(t.Context(), "worker", request)
	if err != nil || len(metadata.Secrets) != 1 {
		t.Fatalf("secret metadata: %v", err)
	}
	raw, _ = json.Marshal(metadata)
	if strings.Contains(string(raw), "secret-stays-on-worker") {
		t.Fatal("secret metadata leaked value")
	}
	request.Action = nodewire.PluginPrepare
	request.Bundle = bundle.Data
	request.Deployment.Node = "other"
	if _, err := registry.Plugins(t.Context(), "worker", request); err == nil {
		t.Fatal("accepted another target node")
	}
}

func TestPluginPreparationChallengesAuthorityBeforePublishing(t *testing.T) {
	server := startNode(t, ServerConfig{Name: "worker", Token: "plugin-test", StateDir: t.TempDir(), PluginAuthorizer: CoordinatorPluginAuthorizer{}})
	registry := NewRegistry("cluster", map[string]Config{"worker": {Addr: server.Addr(), Token: "plugin-test"}})
	t.Cleanup(registry.Close)
	bundle, d := pluginFixture(t, server)
	var checks atomic.Int32
	registry.SetPluginAuthorizer(func(_ context.Context, node string, request nodewire.PluginRequest) error {
		if node != "worker" || request.Authority.CoordinatorEpoch != 2 || checks.Add(1) > 1 {
			return errors.New("stale coordinator")
		}
		return nil
	})
	request := nodewire.PluginRequest{Action: nodewire.PluginPrepare, Authority: nodewire.SessionAuthority{ClusterID: "cluster", CoordinatorNodeID: "coordinator", CoordinatorEpoch: 2, WriterGeneration: 1}, Deployment: d, Bundle: bundle.Data}
	if _, err := registry.Plugins(t.Context(), "worker", request); err == nil {
		t.Fatal("stale publication accepted")
	}
	hash, _ := d.Hash()
	if _, err := server.pluginStore().Deployment(hash); !os.IsNotExist(err) {
		t.Fatalf("published after rejection: %v", err)
	}
	registry.SetPluginAuthorizer(func(context.Context, string, nodewire.PluginRequest) error { return nil })
	reply, err := registry.Plugins(t.Context(), "worker", request)
	if err != nil || reply.Receipt.Hash != hash {
		t.Fatalf("retry after new authorization: %v", err)
	}
	if checks.Load() != 2 {
		t.Fatalf("authorization checks: %d", checks.Load())
	}
}

func TestPluginPrepareFailureDoesNotReportReady(t *testing.T) {
	server := startNode(t, ServerConfig{Name: "worker", Token: "plugin-test", StateDir: t.TempDir()})
	registry := NewRegistry("owner", map[string]Config{"worker": {Addr: server.Addr(), Token: "plugin-test"}})
	t.Cleanup(registry.Close)
	bundle, d := pluginFixture(t, server)
	d.Configuration.Secrets["token"] = plugins.SecretRef{Name: "missing", Revision: strings.Repeat("0", 32)}
	reply, err := registry.Plugins(t.Context(), "worker", nodewire.PluginRequest{Action: nodewire.PluginPrepare, Deployment: d, Bundle: bundle.Data})
	if !errors.Is(err, plugins.ErrUnavailable) || reply.Receipt != nil {
		t.Fatalf("missing credential reported ready: %+v %v", reply, err)
	}
	c, err := registry.connect(t.Context(), "worker")
	if err != nil {
		t.Fatal(err)
	}
	advert := c.getAdvert()
	advert.Features = nil
	c.setAdvert(advert)
	if _, err := registry.Plugins(t.Context(), "worker", nodewire.PluginRequest{Action: nodewire.PluginPrepare, Deployment: d, Bundle: bundle.Data}); !errors.Is(err, plugins.ErrIncompatible) {
		t.Fatalf("old node accepted: %v", err)
	}
}
