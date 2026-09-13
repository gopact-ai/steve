package cluster

import (
	"context"
	"fmt"
	"maps"
	"path/filepath"
	"slices"

	"github.com/gopact-ai/steve/internal/adapter"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/node"
)

// Local process and MCP definitions belong to the physical worker after
// activation; the shared application deliberately excludes them.
func serviceWorker(peer PeerConfig, cfg *config.Config) (node.ServerConfig, error) {
	token, err := ClusterRandomToken()
	if err != nil {
		return node.ServerConfig{}, err
	}
	worker := node.ServerConfig{Name: peer.NodeID, Listen: "127.0.0.1:0", Token: token,
		Hubs: map[string]string{peer.ClusterID: token}, StateDir: filepath.Join(peer.DataDir, "node"),
		WorkspaceRoot: filepath.Dir(cfg.Gateway.StatePath), Harnesses: map[string]node.HarnessSpec{}, MCPServers: map[string]node.MCPSpec{}}
	for id, spec := range cfg.Harnesses {
		worker.Harnesses[id] = node.HarnessSpec{Adapter: spec.Adapter, Command: spec.Command, Args: slices.Clone(spec.Args),
			Env: slices.Clone(spec.Env), ProcessDir: spec.ProcessDir, Slots: spec.Slots}
	}
	for id, spec := range cfg.MCPServers {
		worker.MCPServers[id] = node.MCPSpec{Type: spec.Type, Command: spec.Command, Args: slices.Clone(spec.Args),
			Env: maps.Clone(spec.Env), URL: spec.URL, Headers: maps.Clone(spec.Headers)}
	}
	return worker, nil
}

// A service can initialize before its first run, with no adapter cache yet.
// Resolve pinned adapters on startup, just as the released worker binary does.
func preparePeerAdapters(ctx context.Context, cfg *node.ServerConfig) error {
	installer := &adapter.Installer{Dir: filepath.Join(cfg.StateDir, "adapters")}
	for id, spec := range cfg.Harnesses {
		if spec.Adapter == "" {
			continue
		}
		installed, err := installer.Ensure(ctx, spec.Adapter)
		if err != nil {
			return fmt.Errorf("prepare peer harness %s: %w", id, err)
		}
		spec.Command = installed.Command
		cfg.Harnesses[id] = spec
	}
	return nil
}
