package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/channel/feishu"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/gateway"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/turn"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}

func run(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "setup":
			return setup(args[1:])
		case "doctor":
			return doctor(args[1:])
		case "run":
			args = args[1:]
		}
	}
	return serve(args)
}

func setup(args []string) error {
	flags := flag.NewFlagSet("setup", flag.ContinueOnError)
	configPath := flags.String("config", "config.json", "path to create")
	appID := flags.String("app-id", "", "Lark app id")
	secretEnv := flags.String("app-secret-env", "FEISHU_APP_SECRET", "environment variable containing the Lark app secret")
	allowedSender := flags.String("allowed-sender", "", "allowed Lark sender open_id")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *appID == "" {
		return fmt.Errorf("setup: -app-id is required")
	}
	if *allowedSender == "" {
		return fmt.Errorf("setup: -allowed-sender is required")
	}
	secret := os.Getenv(*secretEnv)
	if secret == "" {
		return fmt.Errorf("setup: environment variable %s is empty", *secretEnv)
	}
	if err := config.Save(*configPath, config.Starter(*appID, secret, *allowedSender)); err != nil {
		return err
	}
	log.Printf("steve: created %s", *configPath)
	return nil
}

func doctor(args []string) error {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	configPath := flags.String("config", "config.json", "path to config file")
	timeout := flags.Duration("timeout", 2*time.Minute, "total harness probe timeout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, catalog, manager, err := load(*configPath)
	if err != nil {
		return err
	}
	defer manager.Stop()
	store, err := state.Open(cfg.Gateway.StatePath)
	if err != nil {
		return err
	}
	if err := store.Check(); err != nil {
		return err
	}
	assembler := cfg.CapabilityAssembler()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	if err := feishu.Check(ctx, cfg.Feishu.AppID, cfg.Feishu.AppSecret); err != nil {
		return err
	}
	for _, selected := range catalog.List() {
		capabilities, err := assembler.Assemble(selected)
		if err != nil {
			return fmt.Errorf("agent %q capabilities: %w", selected.ID, err)
		}
		session, err := manager.OpenSession(ctx, selected.Harness, "", selected.Workspace, capabilities.MCPServers)
		if err != nil {
			return fmt.Errorf("agent %q session: %w", selected.ID, err)
		}
		if err := manager.CloseSession(ctx, selected.Harness, session.ID()); err != nil {
			return fmt.Errorf("agent %q close session: %w", selected.ID, err)
		}
	}
	log.Printf("steve: doctor passed")
	return nil
}

func serve(args []string) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	configPath := flags.String("config", "config.json", "path to config file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, catalog, manager, err := load(*configPath)
	if err != nil {
		return err
	}
	defer manager.Stop()
	store, err := state.Open(cfg.Gateway.StatePath)
	if err != nil {
		return err
	}
	coordinator := turn.New(
		catalog, store, cfg.CapabilityAssembler(), manager, time.Duration(cfg.Gateway.PromptTimeout),
	)
	gw := gateway.New(coordinator)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	channel, err := feishu.New(ctx, cfg.Feishu.AppID, cfg.Feishu.AppSecret, cfg.Feishu.AllowedSenders, gw.HandleMessage)
	if err != nil {
		return err
	}
	gw.BindChannel(channel)

	log.Printf("steve: starting Feishu long connection")
	if err := channel.Start(ctx); err != nil && !errors.Is(err, context.Canceled) && ctx.Err() == nil {
		return fmt.Errorf("start Feishu channel: %w", err)
	}
	return nil
}

func load(path string) (*config.Config, *agent.Catalog, *harness.Manager, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := cfg.Feishu.Validate(); err != nil {
		return nil, nil, nil, err
	}
	catalog, err := cfg.AgentCatalog()
	if err != nil {
		return nil, nil, nil, err
	}
	manager, err := cfg.HarnessManager()
	if err != nil {
		return nil, nil, nil, err
	}
	return cfg, catalog, manager, nil
}
