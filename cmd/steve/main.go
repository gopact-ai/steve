package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/channel/feishu"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/gateway"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/home"
	setupcmd "github.com/gopact-ai/steve/internal/setup"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/turn"
	"golang.org/x/term"
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
		case "pairing":
			return pairing(args[1:])
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
	appID := flags.String("app-id", "", "Feishu/Lark app id")
	secretEnv := flags.String("app-secret-env", "FEISHU_APP_SECRET", "environment variable containing the app secret")
	domain := flags.String("domain", "", "feishu or lark")
	dmPolicy := flags.String("dm-policy", "", "pairing or allowlist")
	allowedSender := flags.String("allowed-sender", "", "allowed sender open_id; implies allowlist if set")
	groupPolicy := flags.String("group-policy", "", "allowlist, open, or disabled")
	allowUnmentioned := flags.Bool("allow-unmentioned", false, "accept group messages without @bot")
	createApp := flags.Bool("create-app", false, "create a Feishu app via the official device-flow link")
	nonInteractive := flags.Bool("non-interactive", false, "do not prompt; require flags and env")
	if err := flags.Parse(args); err != nil {
		return err
	}
	interactive := isTerminal() && !*nonInteractive
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	_, err := setupcmd.Run(ctx, setupcmd.Flags{
		ConfigPath:       *configPath,
		AppID:            *appID,
		SecretEnv:        *secretEnv,
		Domain:           *domain,
		DMPolicy:         *dmPolicy,
		AllowedSender:    *allowedSender,
		GroupPolicy:      *groupPolicy,
		AllowUnmentioned: *allowUnmentioned,
		CreateApp:        *createApp,
	}, setupcmd.Options{Interactive: interactive})
	return err
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
	assembler, err := wireHome(cfg)
	if err != nil {
		return err
	}
	warnHome(cfg)
	if err := checkHome(cfg); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	identity, err := feishu.Probe(ctx, cfg.Feishu.AppID, cfg.Feishu.AppSecret, cfg.Feishu.Domain)
	if err != nil {
		return err
	}
	log.Printf("steve: feishu bot %s %s", identity.Name, identity.OpenID)
	for _, selected := range catalog.List() {
		if _, err := assembler.AssembleMode(selected, home.ModeGuest); err != nil {
			return fmt.Errorf("agent %q guest home: %w", selected.ID, err)
		}
		if cfg.Feishu.OwnerOpenID != "" {
			if _, err := assembler.AssembleMode(selected, home.ModeOwner); err != nil {
				return fmt.Errorf("agent %q owner home: %w", selected.ID, err)
			}
		}
		capabilities, err := assembler.AssembleMode(selected, home.ModeGuest)
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
	assembler, err := wireHome(cfg)
	if err != nil {
		return err
	}
	warnHome(cfg)
	for _, selected := range catalog.List() {
		if _, err := assembler.AssembleMode(selected, home.ModeGuest); err != nil {
			return fmt.Errorf("agent %q guest home: %w", selected.ID, err)
		}
		if cfg.Feishu.OwnerOpenID != "" {
			if _, err := assembler.AssembleMode(selected, home.ModeOwner); err != nil {
				return fmt.Errorf("agent %q owner home: %w", selected.ID, err)
			}
		}
	}
	coordinator := turn.New(
		catalog, store, assembler, manager, time.Duration(cfg.Gateway.PromptTimeout),
	)
	coordinator.SetIdentity(cfg.Feishu.OwnerOpenID, home.Dir{Path: cfg.Gateway.HomePath})
	gw := gateway.New(coordinator)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		stop()
	}()
	channel, err := feishu.New(ctx, feishu.Options{
		AppID:            cfg.Feishu.AppID,
		AppSecret:        cfg.Feishu.AppSecret,
		Domain:           cfg.Feishu.Domain,
		Access:           feishu.AccessFrom(cfg.Feishu, nil),
		Pairing:          store,
		ExtraAllows:      store.Allows,
		AllowUnmentioned: cfg.Feishu.AllowUnmentioned,
	}, gw.HandleMessage)
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

func pairing(args []string) error {
	flags := flag.NewFlagSet("pairing", flag.ContinueOnError)
	configPath := flags.String("config", "config.json", "path to config file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	rest := flags.Args()
	if len(rest) == 0 {
		return fmt.Errorf("usage: steve pairing list|approve CODE")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if err := cfg.Feishu.Validate(); err != nil {
		return err
	}
	store, err := state.Open(cfg.Gateway.StatePath)
	if err != nil {
		return err
	}
	switch rest[0] {
	case "list":
		pending, approved := store.PairingList()
		if len(pending) == 0 && len(approved) == 0 {
			fmt.Println("no pairing requests")
			return nil
		}
		for _, item := range pending {
			fmt.Printf("pending %s %s\n", item.Code, item.OpenID)
		}
		for _, openID := range approved {
			fmt.Printf("approved %s\n", openID)
		}
		return nil
	case "approve":
		if len(rest) < 2 {
			return fmt.Errorf("usage: steve pairing approve CODE")
		}
		openID, err := store.ApprovePairing(rest[1])
		if err != nil {
			return err
		}
		fmt.Printf("approved %s\n", openID)
		return nil
	default:
		return fmt.Errorf("usage: steve pairing list|approve CODE")
	}
}

func wireHome(cfg *config.Config) (*capability.Assembler, error) {
	if err := home.Bootstrap(cfg.Gateway.HomePath, cfg.Feishu.OwnerOpenID); err != nil {
		return nil, err
	}
	return cfg.CapabilityAssembler().SetHome(home.Dir{Path: cfg.Gateway.HomePath}), nil
}

func warnHome(cfg *config.Config) {
	if cfg.Feishu.OwnerOpenID == "" {
		log.Printf("steve: feishu.owner_open_id is unset; DMs use guest home")
	}
}

func checkHome(cfg *config.Config) error {
	info, err := os.Stat(cfg.Gateway.HomePath)
	if err != nil {
		return err
	}
	if info.Mode().Perm() != 0o700 {
		log.Printf("steve: home directory mode is %o, want 700", info.Mode().Perm())
	}
	for _, name := range []string{home.FileSoul, home.FileUser, home.FileMemory} {
		path := filepath.Join(cfg.Gateway.HomePath, name)
		st, err := os.Stat(path)
		if err != nil {
			return err
		}
		if st.Mode().Perm() != 0o600 {
			log.Printf("steve: %s mode is %o, want 600", name, st.Mode().Perm())
		}
	}
	snap, err := home.Load(cfg.Gateway.HomePath, home.ModeOwner)
	if err != nil {
		return err
	}
	for _, warning := range snap.Warnings {
		log.Printf("steve: %s", warning)
	}
	user, err := os.ReadFile(filepath.Join(cfg.Gateway.HomePath, home.FileUser))
	if err != nil {
		return err
	}
	if strings.Contains(string(user), home.TemplateMarker) {
		log.Printf("steve: edit USER.md")
	}
	return nil
}

func isTerminal() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}
