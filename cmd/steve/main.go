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

	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/channel/feishu"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/gateway"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	configPath := flag.String("config", "config.json", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if err := cfg.Feishu.Validate(); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	host := acphost.New(acphost.Config{
		Command:    cfg.Agent.Command,
		Args:       cfg.Agent.Args,
		Workdir:    cfg.Agent.Workdir,
		Env:        cfg.Agent.Env,
		Permission: cfg.Agent.Permission,
	})
	defer host.Stop()

	gw := gateway.New(gateway.Config{PromptTimeout: time.Duration(cfg.Gateway.PromptTimeout)}, host)
	channel := feishu.New(cfg.Feishu.AppID, cfg.Feishu.AppSecret, gw.HandleMessage)
	gw.BindChannel(channel)

	log.Printf("steve: starting Feishu long connection")
	if err := channel.Start(ctx); err != nil && !errors.Is(err, context.Canceled) && ctx.Err() == nil {
		return fmt.Errorf("start Feishu channel: %w", err)
	}
	return nil
}
