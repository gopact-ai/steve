// Package configbuild turns a loaded configuration into the runtime
// components and project records it declares: fetched adapters, the harness
// manager, the capability assembler, node dial settings and the project
// projection in the ledger. The config package only describes and checks
// the file, so it does not depend on the packages that run it.
package configbuild

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/gopact-ai/steve/internal/adapter"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/node"
)

// PrepareAdapters fetches and verifies every adapter the configuration
// names, filling in the command that starts it. It runs before the harness
// manager is built, so a machine either has the pinned adapter or refuses
// to start with a reason — there is no version to discover later.
func PrepareAdapters(ctx context.Context, c *config.Config) error {
	install := &adapter.Installer{Dir: c.AdapterDir()}
	for id, item := range c.Harnesses {
		if item.Adapter == "" {
			continue
		}
		got, err := install.Ensure(ctx, item.Adapter)
		if err != nil {
			return fmt.Errorf("harness %q: %w", id, err)
		}
		if !got.Cached {
			slog.Info(fmt.Sprintf("adapter: installed %s@%s for harness %s", got.Package, got.Version, id), "harness", id)
		}
		item.Command = got.Command
		c.Harnesses[id] = item
	}
	return nil
}

func HarnessManager(c *config.Config) (*harness.Manager, error) {
	configs := make(map[string]harness.Config, len(c.Harnesses))
	for id, item := range c.Harnesses {
		configs[id] = harness.Config{
			Command: item.Command, Args: item.Args, ProcessDir: item.ProcessDir, Env: item.Env, Permission: item.Permission,
		}
	}
	manager, err := harness.NewManager(configs)
	if err != nil {
		return nil, err
	}
	if err := manager.SetRemotePermissions(c.RuntimePermissions); err != nil {
		return nil, err
	}
	return manager, nil
}

// NodeConfigs is what the registry needs to reach each remote machine.
func NodeConfigs(c *config.Config) map[string]node.Config {
	out := make(map[string]node.Config, len(c.Nodes))
	for id, item := range c.Nodes {
		out[id] = node.Config{Addr: item.Addr, Token: item.Token, DialTimeout: time.Duration(item.Dial), Level: item.Level, Region: item.Region, PeerAddr: item.PeerAddr}
	}
	return out
}

func CapabilityAssembler(c *config.Config) *capability.Assembler {
	servers := make(map[string]capability.MCPServer, len(c.MCPServers))
	for id, item := range c.MCPServers {
		servers[id] = capability.MCPServer{
			Type: item.Type, Command: item.Command, Args: item.Args, Env: item.Env, URL: item.URL, Headers: item.Headers,
		}
	}
	return capability.NewAssembler(servers)
}
