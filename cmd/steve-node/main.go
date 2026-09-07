// Command steve-node runs agents on one machine on behalf of a Steve hub.
//
// The coordinator owns tasks. The node owns ACP sessions, harness processes,
// and durable receipts so a lost coordinator socket need not end execution.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gopact-ai/steve/internal/adapter"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/processrestart"
)

func main() {
	log.SetFlags(log.LstdFlags)
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "steve-node: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) > 0 && args[0] == node.LaunchVerb {
		return launch(args[1:])
	}
	if len(args) > 0 && args[0] == "adopt" {
		return adopt(args[1:])
	}
	if len(args) > 0 && args[0] == "mcp-broker" {
		return broker(args[1:])
	}
	flags := flag.NewFlagSet("steve-node", flag.ContinueOnError)
	configPath := flags.String("config", "node.json", "path to the node config file")
	listen := flags.String("listen", "", "override the configured listen address")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := load(*configPath)
	if err != nil {
		return err
	}
	cfg.Source = absolute(*configPath)
	if *listen != "" {
		cfg.Listen = *listen
	}
	if value := os.Getenv("STEVE_NODE_SESSION_GRACE"); value != "" {
		cfg.SessionGrace, err = time.ParseDuration(value)
		if err != nil || cfg.SessionGrace <= 0 {
			return fmt.Errorf("STEVE_NODE_SESSION_GRACE must be a positive duration")
		}
	}
	if value := os.Getenv("STEVE_NODE_FAULT"); value != "" {
		delay, ok := strings.CutPrefix(value, "drop-hub-after:")
		if !ok {
			return fmt.Errorf("unknown STEVE_NODE_FAULT %q", value)
		}
		cfg.FaultDropAfter, err = time.ParseDuration(delay)
		if err != nil || cfg.FaultDropAfter <= 0 {
			return fmt.Errorf("drop-hub-after requires a positive duration")
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := prepareAdapters(ctx, &cfg); err != nil {
		return err
	}
	cfg.SessionAuthorizer = node.CoordinatorSessionAuthorizer{}
	server := node.NewServer(cfg)
	if processrestart.Supported() {
		server.SetRestartCheck(func() error { return checkRestartConfig(cfg, *listen) })
		server.EnableRestart()
	}
	err = server.Serve(ctx)
	if !errors.Is(err, node.ErrRestartRequested) {
		return err
	}
	if ctx.Err() != nil {
		return nil
	}
	if err := processrestart.ReexecCurrent(); err != nil {
		return errors.Join(err, server.RestartFailed(err))
	}
	return nil
}

// broker runs the MCP broker as its own process: "steve-node mcp-broker
// -config mcp.json". Run it as its own user with mcp.json readable by that
// user alone, and point node.json's mcp_broker at its socket and token.
func broker(args []string) error {
	flags := flag.NewFlagSet("steve-node mcp-broker", flag.ContinueOnError)
	configPath := flags.String("config", "mcp.json", "path to the broker config file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	raw, err := os.ReadFile(*configPath)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	var cfg node.BrokerConfig
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	if cfg.Socket == "" || cfg.Token == "" {
		return fmt.Errorf("mcp-broker: socket and token are required")
	}
	cfg.Socket = absolute(cfg.Socket)
	cfg.PortFile = absolute(cfg.PortFile)
	cfg.WorkspaceRoot = absolute(cfg.WorkspaceRoot)
	if info, err := os.Stat(*configPath); err == nil && info.Mode().Perm()&0o077 != 0 {
		log.Printf("steve-node: %s is readable by others (mode %o); the secrets in it are not only yours", *configPath, info.Mode().Perm())
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return node.NewBroker(cfg).Serve(ctx)
}

// adopt hands this node to a named hub: "steve-node adopt -config node.json <hub>".
func adopt(args []string) error {
	flags := flag.NewFlagSet("steve-node adopt", flag.ContinueOnError)
	configPath := flags.String("config", "node.json", "path to the node config file")
	evidence := flags.String("evidence", "", "operator verified physical termination of unresolved old processes")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("usage: steve-node adopt [-config node.json] <hub>")
	}
	cfg, err := load(*configPath)
	if err != nil {
		return err
	}
	if err := node.AdoptWithEvidence(cfg.StateDir, flags.Arg(0), "operator", *evidence); err != nil {
		return err
	}
	fmt.Printf("%s now belongs to hub %q\n", cfg.Name, flags.Arg(0))
	return nil
}

// launch is the MCP launcher an agent runs: it carries a binding id and a
// socket, never a secret, and pipes the agent to the server the node's
// broker starts for that binding.
func launch(args []string) error {
	flags := flag.NewFlagSet("steve-node mcp-launch", flag.ContinueOnError)
	socket := flags.String("socket", "", "the node's MCP broker socket")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("usage: steve-node %s -socket <path> <binding>", node.LaunchVerb)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return node.LaunchBinding(ctx, *socket, flags.Arg(0), os.Stdin, os.Stdout)
}

func load(path string) (node.ServerConfig, error) {
	snapshot, err := readNodeConfig(path)
	return snapshot.config, err
}

func decodeNodeConfig(raw []byte) (node.ServerConfig, error) {
	var cfg node.ServerConfig
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return node.ServerConfig{}, fmt.Errorf("parse config: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return node.ServerConfig{}, errors.New("config must contain exactly one JSON document")
	}
	if strings.TrimSpace(cfg.Name) == "" {
		return node.ServerConfig{}, fmt.Errorf("name is required — it is what the hub records on every attempt")
	}
	// The token is the node's own authentication of the hub, deliberately
	// separate from whatever the network layer does: two independent
	// defences should not share one failure.
	if strings.TrimSpace(cfg.Token) == "" {
		return node.ServerConfig{}, fmt.Errorf("token is required")
	}
	if cfg.Listen == "" {
		cfg.Listen = "0.0.0.0:7701"
	}
	cfg.WorkspaceRoot = absolute(cfg.WorkspaceRoot)
	cfg.StateDir = absolute(cfg.StateDir)
	if cfg.StateDir == "" {
		cfg.StateDir = absolute("~/.steve-node")
	}
	for id, spec := range cfg.Harnesses {
		spec.ProcessDir = absolute(spec.ProcessDir)
		switch {
		case spec.Adapter != "" && spec.Command != "":
			return node.ServerConfig{}, fmt.Errorf("harness %q sets both adapter and command; pick one", id)
		case spec.Adapter != "" && len(spec.Args) > 0:
			return node.ServerConfig{}, fmt.Errorf("harness %q: an adapter takes no args", id)
		case spec.Adapter != "":
			if _, known := adapter.Catalog[spec.Adapter]; !known {
				return node.ServerConfig{}, fmt.Errorf("harness %q: adapter %q is not one of %s", id, spec.Adapter, strings.Join(adapter.Names(), ", "))
			}
		}
		cfg.Harnesses[id] = spec
	}
	return cfg, nil
}

// prepareAdapters fetches and verifies every adapter this machine's config
// names, filling in the command that starts it. A node that cannot get the
// pinned version does not come up: an agent running some other version is
// worse than a machine that says why it is missing.
func prepareAdapters(ctx context.Context, cfg *node.ServerConfig) error {
	install := &adapter.Installer{Dir: filepath.Join(cfg.StateDir, "adapters")}
	for id, spec := range cfg.Harnesses {
		if spec.Adapter == "" {
			continue
		}
		got, err := install.Ensure(ctx, spec.Adapter)
		if err != nil {
			return fmt.Errorf("harness %q: %w", id, err)
		}
		if !got.Cached {
			log.Printf("steve-node: installed %s@%s for harness %s", got.Package, got.Version, id)
		}
		spec.Command = got.Command
		cfg.Harnesses[id] = spec
	}
	return nil
}

func absolute(path string) string {
	if path == "" {
		return ""
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/"))
		}
	}
	result, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return result
}
