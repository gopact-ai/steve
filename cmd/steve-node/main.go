// Command steve-node runs agents on one machine on behalf of a Steve hub.
//
// It is deliberately thin: it holds no memory, no task state and no identity.
// The hub assembles every session's context and owns the task tree; the node
// starts processes and shuttles bytes. That is what makes a node replaceable
// — losing one costs the sessions it was running, nothing more.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/gopact-ai/steve/internal/node"
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
	if *listen != "" {
		cfg.Listen = *listen
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return node.NewServer(cfg).Serve(ctx)
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
	raw, err := os.ReadFile(path)
	if err != nil {
		return node.ServerConfig{}, fmt.Errorf("read config: %w", err)
	}
	var cfg node.ServerConfig
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return node.ServerConfig{}, fmt.Errorf("parse config: %w", err)
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
	if len(cfg.Harnesses) == 0 {
		return node.ServerConfig{}, fmt.Errorf("at least one harness is required")
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
		cfg.Harnesses[id] = spec
	}
	return cfg, nil
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
