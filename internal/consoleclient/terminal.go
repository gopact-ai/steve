package consoleclient

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gopact-ai/steve/internal/clientsdk"
	"github.com/gopact-ai/steve/internal/desktop"
	"github.com/gopact-ai/steve/internal/tuiapp"
)

// Terminal is `steve tui`, and `steve` on a terminal: the interactive
// client. It connects the way the other console clients do when -config,
// -url or -token is given; otherwise it uses this user's local
// installation, starting its service the way `steve desktop` does.
func Terminal(args []string) error {
	flags := flag.NewFlagSet("tui", flag.ContinueOnError)
	connectionFlags := addConsoleClientFlags(flags)
	stateDir := flags.String("state-dir", "", "local installation directory (default: the per-user application support directory)")
	thread := flags.String("thread", "", "thread to open, created when it does not exist (default: the most recent console thread)")
	project := flags.String("project", "", "project of a thread this client creates (default: the server's default project)")
	timeout := flags.Duration("timeout", 30*time.Second, "maximum time to wait for the local service to start")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: steve tui [-thread …] [-project …] [-state-dir …] [-config … | -url … -token …]")
	}
	explicit := false
	flags.Visit(func(f *flag.Flag) {
		explicit = explicit || f.Name == "config" || f.Name == "url" || f.Name == "token"
	})
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	var connection consoleConnection
	var notice string
	var err error
	if explicit {
		connection, err = connectionFlags.resolve(flags)
	} else {
		connection, notice, err = localConnection(ctx, *stateDir, *timeout)
	}
	if err != nil {
		return err
	}
	client := clientsdk.New(connection.URL, connection.Token, consoleHTTPClient())
	client.Language = acceptLanguage(os.Getenv("LC_ALL"), os.Getenv("LANG"))
	check, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := client.Initialize(check); err != nil {
		if errors.Is(err, clientsdk.ErrNotV1) {
			return fmt.Errorf("the service at %s is a Steve build without the terminal client protocol; upgrade it, then run steve tui again", connection.URL)
		}
		return fmt.Errorf("connect to %s: %w", connection.URL, err)
	}
	return tuiapp.Run(ctx, client, tuiapp.Options{Thread: *thread, Project: *project, Notice: notice}, os.Stdin, os.Stdout)
}

// acceptLanguage turns the first POSIX locale set, such as en_US.UTF-8,
// into a language tag the server reads, such as en-US. C and POSIX name
// no language, and the server then answers in its own.
func acceptLanguage(locales ...string) string {
	for _, locale := range locales {
		tag, _, _ := strings.Cut(locale, ".")
		tag, _, _ = strings.Cut(tag, "@")
		if tag == "" || tag == "C" || tag == "POSIX" {
			continue
		}
		return strings.ReplaceAll(tag, "_", "-")
	}
	return ""
}

// localConnection finds this user's installation, made by `steve init` or
// the desktop app, and makes sure its service is running. It creates no
// installation: that is `steve init`'s job, which also registers agents.
func localConnection(ctx context.Context, stateDir string, timeout time.Duration) (consoleConnection, string, error) {
	root := stateDir
	if root == "" {
		var err error
		if root, err = desktop.DefaultStateDir(); err != nil {
			return consoleConnection{}, "", err
		}
	}
	if !desktop.IsManagedConfig(filepath.Join(root, "config.json")) {
		return consoleConnection{}, "", fmt.Errorf("no local Steve installation in %s: run steve init first, or pass -config to connect to a service started with steve run", root)
	}
	installed, err := desktop.Bootstrap(desktop.Options{StateDir: root})
	if err != nil {
		return consoleConnection{}, "", err
	}
	executable, err := os.Executable()
	if err != nil {
		return consoleConnection{}, "", err
	}
	start, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result, err := desktop.EnsureRunning(start, installed, executable)
	if err != nil {
		return consoleConnection{}, "", err
	}
	notice := ""
	if result.Started {
		notice = "Started the local Steve service"
	}
	if upgrade := result.Upgrade; upgrade != nil && !upgrade.Applied {
		notice = fmt.Sprintf("The service is running %s; it restarts on %s once its work has finished", upgrade.From, upgrade.To)
		if upgrade.Error != "" {
			notice = fmt.Sprintf("The service is running %s and could not be brought to %s", upgrade.From, upgrade.To)
		}
	}
	return consoleConnection{URL: result.URL, Token: installed.Token}, notice, nil
}
