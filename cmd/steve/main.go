package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	neturl "net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	adminsvc "github.com/gopact-ai/steve/internal/admin"
	"github.com/gopact-ai/steve/internal/agenttools"
	"github.com/gopact-ai/steve/internal/app"
	"github.com/gopact-ai/steve/internal/processrestart"
	setupcmd "github.com/gopact-ai/steve/internal/setup"
	"github.com/gopact-ai/steve/internal/tui"
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

func run(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "setup":
			return setup(args[1:])
		case "doctor":
			return doctor(args[1:])
		case "top":
			return top(os.Args[2:])
		case "dash":
			return dash(os.Args[2:])
		case "desktop":
			return desktopCmd(args[1:])
		case "peer":
			return peerCmd(args[1:])
		case "peer-import":
			return peerImportCmd(args[1:])
		case "ledger":
			return ledgerCmd(args[1:])
		case "migrate":
			return migrateCmd(args[1:])
		case "say":
			return say(args[1:])
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

// top renders the read model in this terminal. It is a client of the running
// gateway's HTTP surface, not a second reader of the stores: one read model,
// two renderers, so the terminal and the browser cannot disagree.
func top(args []string) error {
	flags := flag.NewFlagSet("top", flag.ContinueOnError)
	url := flags.String("url", defaultReadModelURL, "read model URL of a running gateway")
	token := flags.String("token", "", "token, when the read model is not on loopback")
	refresh := flags.Duration("refresh", 5*time.Second, "redraw floor; changes also redraw immediately")
	once := flags.Bool("once", false, "print one frame and exit")
	if err := flags.Parse(args); err != nil {
		return err
	}
	model := tui.New(tui.Config{URL: *url, Token: *token, Refresh: *refresh})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *once {
		fmt.Print(model.Once(ctx))
		return nil
	}
	return model.Run(ctx)
}

// dash prints the dashboard URL of a running gateway. The page is served by
// the gateway itself, so there is no second process to keep alive.
func dash(args []string) error {
	flags := flag.NewFlagSet("dash", flag.ContinueOnError)
	url := flags.String("url", defaultReadModelURL, "read model URL of a running gateway")
	token := flags.String("token", "", "token, when the read model is not on loopback")
	if err := flags.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, *url+"/state", nil)
	if err != nil {
		return err
	}
	if *token != "" {
		req.Header.Set("Authorization", "Bearer "+*token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("no gateway at %s — is `steve run` up? %w", *url, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("read model at %s answered %s", *url, res.Status)
	}
	page := *url
	if *token != "" {
		page += "/?token=" + neturl.QueryEscape(*token)
	}
	fmt.Println(page)
	return nil
}

// defaultReadModelURL is where `steve run` puts the read model unless the
// config says otherwise.
const defaultReadModelURL = "http://127.0.0.1:7710"

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

// say sends one line to a running gateway's console and prints the reply:
// the page's send box, from a shell.
func say(args []string) error {
	flags := flag.NewFlagSet("say", flag.ContinueOnError)
	url := flags.String("url", defaultReadModelURL, "read model URL of a running gateway")
	token := flags.String("token", "", "token, when the read model is not on loopback")
	conversation := flags.String("conversation", "console:main", "console conversation")
	if err := flags.Parse(args); err != nil {
		return err
	}
	input := strings.TrimSpace(strings.Join(flags.Args(), " "))
	if input == "" {
		return errors.New("usage: steve say [-url …] [-token …] <text or /verb …>")
	}
	body, _ := json.Marshal(map[string]string{"conversation": *conversation, "input": input})
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, *url+"/console/send", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if *token != "" {
		req.Header.Set("Authorization", "Bearer "+*token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("no gateway at %s — is `steve run` up? %w", *url, err)
	}
	defer res.Body.Close()
	var out struct {
		Error string `json:"error"`
		Reply struct {
			Title string `json:"title"`
			Text  string `json:"text"`
		} `json:"reply"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return fmt.Errorf("console at %s answered %s", *url, res.Status)
	}
	if out.Reply.Title != "" {
		fmt.Println("== " + out.Reply.Title)
	}
	fmt.Println(out.Reply.Text)
	if out.Error != "" {
		return errors.New(out.Error)
	}
	return nil
}
