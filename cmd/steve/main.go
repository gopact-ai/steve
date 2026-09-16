package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"os"
	"time"

	adminsvc "github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/agenttools"
	"github.com/gopact-ai/steve/internal/app"
	"github.com/gopact-ai/steve/internal/consoleclient"
	"github.com/gopact-ai/steve/internal/processrestart"
	setupcmd "github.com/gopact-ai/steve/internal/setup"
	"golang.org/x/term"
)

func main() {
	if err := agenttools.InitializePath(); err != nil {
		log.Fatal(err)
	}
	if err := run(os.Args[1:]); err != nil {
		var restart *adminsvc.RestartExit
		if errors.As(err, &restart) {
			err = processrestart.ReexecCurrent()
			if err != nil {
				restart.Service.Failed(err)
			}
		}
		log.Fatal(err)
	}
}

// commands are the subcommands by name; anything else is `steve run`. A table
// rather than a switch: it gives back the 10 lines the ledger verb split cost.
var commands = map[string]func(args []string) error{
	"setup": setup, "doctor": doctor, "top": consoleclient.Top, "dash": consoleclient.Dash, "desktop": desktopCmd, "peer": peerCmd, "peer-import": peerImportCmd, "peer-init": app.InitClusterCommand, "link": linkCmd,
	"ledger": ledgerCmd, "migrate": migrateCmd, "say": consoleclient.Say, "plugins": app.PluginsCommand, "mcp-launch": app.MCPLaunchCommand,
}

func run(args []string) error {
	if len(args) > 0 {
		if command, ok := commands[args[0]]; ok {
			return command(args[1:])
		}
		if args[0] == "run" {
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
	allowedSender := flags.String("allowed-sender", "", "optional group allowlist open_id; empty means open")
	blockedSender := flags.String("blocked-sender", "", "blocked sender open_id")
	groupPolicy := flags.String("group-policy", "", "open, allowlist, or disabled")
	allowUnmentioned := flags.Bool("allow-unmentioned", false, "listen to group messages without @ and let Steve decide whether to reply")
	ownerOpenID := flags.String("owner-open-id", "", "owner open_id (ou_...); the owner's DMs load the owner home and steve run opens the home chat")
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
		AllowedSender:    *allowedSender,
		BlockedSender:    *blockedSender,
		GroupPolicy:      *groupPolicy,
		AllowUnmentioned: *allowUnmentioned,
		CreateApp:        *createApp,
		OwnerOpenID:      *ownerOpenID,
	}, setupcmd.Options{Interactive: interactive})
	return err
}

func serve(args []string) error {
	if handled, err := maybeManagedPeer(args); handled {
		return err
	}
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	configPath := flags.String("config", "config.json", "path to config file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	application, err := app.Build(context.Background(), app.Config{Path: *configPath})
	if err != nil {
		return err
	}
	return application.Run(context.Background())
}

func isTerminal() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}
