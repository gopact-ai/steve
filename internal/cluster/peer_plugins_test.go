package cluster

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/platformconfig"
	"github.com/gopact-ai/steve/internal/plugins"
)

func TestPeerPluginAuthorizationRequiresCurrentWriterAndExactDeclaration(t *testing.T) {
	options, installed := testPeerOptions(t, ClusterPeerTestDir(t), nil)
	options.Activate = testPeerApplication(t, new(atomic.Int32))
	peer := StartTestPeer(t, options)
	active := WaitPeerReady(t, peer)
	cfg, err := config.Load(installed.Paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Plugins = map[string]plugins.Installation{"tools": {PackageID: "example/team-tools", Digest: strings.Repeat("a", 64), Projects: []string{"workspace"}, Targets: map[string]plugins.Configuration{"": {Values: map[string]string{"endpoint": "https://example.invalid"}}}}}
	shared := platformconfig.New(active.Ledger)
	declaration, err := shared.Bootstrap(t.Context(), cfg, platformconfig.LocalNode{ID: peer.Config.NodeID, Config: config.Node{Addr: peer.Worker().Address, Token: peer.Worker().Token, Level: "restricted"}})
	if err != nil {
		t.Fatal(err)
	}
	deployment, err := declaration.Plugins["tools"].Deployment("tools", peer.Config.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	request := nodewire.PluginRequest{Action: nodewire.PluginPrepare, Node: peer.Config.NodeID, Authority: nodewire.SessionAuthority{ClusterID: peer.Config.ClusterID, CoordinatorNodeID: peer.Config.NodeID, CoordinatorEpoch: active.Assignment.Epoch, WriterGeneration: active.WriterGeneration}, Deployment: deployment}
	if err := peer.AuthorizePlugins(t.Context(), peer.Config.NodeID, request); err != nil {
		t.Fatal(err)
	}
	authorize := peer.ApplicationPluginAuthorizer(active)
	if err := authorize(t.Context(), peer.Config.NodeID, request); err != nil {
		t.Fatal(err)
	}
	stale := request
	stale.Authority.WriterGeneration++
	if err := authorize(t.Context(), peer.Config.NodeID, stale); !errors.Is(err, coordination.ErrStaleEpoch) {
		t.Fatalf("writer generation: %v", err)
	}
	if err := peer.AuthorizePlugins(t.Context(), "imposter", request); err == nil {
		t.Fatal("accepted wrong TLS peer")
	}
	changed := request
	changed.Deployment.Configuration = deployment.Configuration.Clone()
	changed.Deployment.Configuration.Values["endpoint"] = "https://other.invalid"
	if err := authorize(t.Context(), peer.Config.NodeID, changed); !errors.Is(err, plugins.ErrConflict) {
		t.Fatalf("changed configuration: %v", err)
	}
	item := declaration.Plugins["tools"]
	item.Digest = strings.Repeat("b", 64)
	declaration.Plugins["tools"] = item
	if _, err := shared.Save(t.Context(), declaration.Revision, declaration); err != nil {
		t.Fatal(err)
	}
	if err := authorize(t.Context(), peer.Config.NodeID, request); !errors.Is(err, plugins.ErrConflict) {
		t.Fatalf("superseded declaration accepted: %v", err)
	}
	cancelled, cancel := context.WithCancel(active.Context)
	cancel()
	active.Context = cancelled
	if err := peer.ApplicationPluginAuthorizer(active)(t.Context(), peer.Config.NodeID, request); !errors.Is(err, context.Canceled) {
		t.Fatalf("ended coordinator activation: %v", err)
	}
}
