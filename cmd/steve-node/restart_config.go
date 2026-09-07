package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"path/filepath"
	"strconv"

	"github.com/gopact-ai/steve/internal/adapter"
	"github.com/gopact-ai/steve/internal/node"
)

type nodeConfigSnapshot struct {
	path   string
	config node.ServerConfig
	digest [32]byte
}

func readNodeConfig(path string) (nodeConfigSnapshot, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nodeConfigSnapshot{}, fmt.Errorf("read config: %w", err)
	}
	cfg, err := decodeNodeConfig(raw)
	if err != nil {
		return nodeConfigSnapshot{}, err
	}
	return nodeConfigSnapshot{path: path, config: cfg, digest: sha256.Sum256(raw)}, nil
}

func (snapshot nodeConfigSnapshot) unchanged() error {
	raw, err := os.ReadFile(snapshot.path)
	if err != nil {
		return fmt.Errorf("recheck configuration: %w", err)
	}
	if sha256.Sum256(raw) != snapshot.digest {
		return errors.New("configuration changed during restart validation; retry")
	}
	return nil
}

func checkRestartConfig(running node.ServerConfig, listenOverride string) error {
	snapshot, err := readNodeConfig(running.Source)
	if err != nil {
		return err
	}
	next := snapshot.config
	if listenOverride != "" {
		next.Listen = listenOverride
	}
	// A service-control restart must reconnect to the same instance. Moving
	// its endpoint, state or authenticated identity is a separate operation.
	switch {
	case next.Name != running.Name:
		return errors.New("node name changed; controlled restart requires the same identity")
	case next.Listen != running.Listen:
		return errors.New("listen address changed; controlled restart requires the same endpoint")
	case next.StateDir != running.StateDir:
		return errors.New("state directory changed; controlled restart requires the same instance")
	case next.Token != running.Token || !maps.Equal(next.Hubs, running.Hubs):
		return errors.New("hub authentication changed; controlled restart requires the same owner access")
	}
	_, port, err := net.SplitHostPort(next.Listen)
	if err != nil {
		return fmt.Errorf("invalid listen address: %w", err)
	}
	if number, err := strconv.Atoi(port); err == nil && number == 0 {
		return errors.New("an automatically allocated listen port cannot be preserved across restart")
	}
	if next.MCPBroker != nil && next.MCPBroker.Socket != "" && len(next.MCPServers) > 0 {
		return errors.New("mcp_servers and mcp_broker are exclusive")
	}
	// Resolve performs only cache inspection. Ensure would download/install
	// and must never run in this preflight while the healthy service is live.
	cache := &adapter.Installer{Dir: filepath.Join(next.StateDir, "adapters")}
	for id, spec := range next.Harnesses {
		if spec.Adapter == "" {
			continue
		}
		if _, ready := cache.Resolve(spec.Adapter); !ready {
			return fmt.Errorf("harness %s needs an adapter installation; prepare it before restarting", id)
		}
	}
	return snapshot.unchanged()
}
