package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/home"
)

func main() {
	configPath := flag.String("config", "config.json", "path to config file")
	agentID := flag.String("agent", "", "agent id or alias")
	timeout := flag.Duration("timeout", 10*time.Minute, "prompt timeout")
	flag.Parse()
	if strings.TrimSpace(strings.Join(flag.Args(), " ")) == "" {
		log.Fatal("usage: acptest [-config config.json] [-agent id] <prompt>")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	catalog, err := cfg.AgentCatalog()
	if err != nil {
		log.Fatal(err)
	}
	selected := catalog.Default()
	if *agentID != "" {
		var ok bool
		selected, ok = catalog.Resolve(*agentID)
		if !ok {
			log.Fatalf("unknown agent %q", *agentID)
		}
	}
	assembler := cfg.CapabilityAssembler()
	mode := home.ModeGuest
	if _, err := os.Stat(cfg.Gateway.HomePath); err == nil {
		assembler.SetHome(home.Dir{Path: cfg.Gateway.HomePath})
		if cfg.Feishu.OwnerOpenID != "" {
			mode = home.ModeOwner
		}
	}
	capabilities, err := assembler.AssembleMode(selected, mode)
	if err != nil {
		log.Fatal(err)
	}
	manager, err := cfg.HarnessManager()
	if err != nil {
		log.Fatal(err)
	}
	defer manager.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	session, err := manager.OpenSession(ctx, selected.Harness, "", selected.Workspace, capabilities.MCPServers)
	if err != nil {
		log.Fatal(err)
	}
	prompt := strings.TrimSpace(strings.Join(flag.Args(), " "))
	if capabilities.Instructions != "" {
		prompt = capabilities.Instructions + "\n\n" + prompt
	}
	out, _, err := session.Prompt(ctx, prompt)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(out)
}
