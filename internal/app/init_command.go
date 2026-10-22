package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/desktop"
	"github.com/gopact-ai/steve/internal/nodewire"
)

// ReleaseVersion is set by release builds through -ldflags. Source builds
// report the module build information alone.
var ReleaseVersion = ""

// VersionCommand is `steve version`.
func VersionCommand([]string) error {
	build := nodewire.Version()
	if ReleaseVersion != "" {
		build = ReleaseVersion + " (" + build + ")"
	}
	_, err := fmt.Fprintf(os.Stdout, "steve %s %s/%s\n", build, runtime.GOOS, runtime.GOARCH)
	return err
}

// InitCommand is `steve init`, the first run: one local installation shared
// with the desktop app, the coding agents this machine is already signed in
// to, and a running console.
func InitCommand(args []string) error {
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	stateDir := flags.String("state-dir", "", "private data directory (default: the per-user application support directory)")
	noStart := flags.Bool("no-start", false, "prepare the installation without starting the service")
	timeout := flags.Duration("timeout", 30*time.Second, "maximum time to wait for the local service")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected init arguments")
	}
	installed, err := desktop.Bootstrap(desktop.Options{StateDir: *stateDir})
	if err != nil {
		return err
	}
	if *noStart {
		next := "steve init"
		if *stateDir != "" {
			next += " -state-dir " + shellQuote(*stateDir)
		}
		fmt.Fprintf(os.Stdout, "Prepared %s. Finish with: %s\n", installed.Paths.Root, next)
		return nil
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	result, err := desktop.EnsureRunning(ctx, installed, executable)
	if err != nil {
		return err
	}
	// Adapters may need a download; enrollment gets its own budget.
	enrollCtx, cancelEnroll := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancelEnroll()
	enrolled, err := desktop.EnrollRunning(enrollCtx, installed, result.URL)
	if err != nil {
		return fmt.Errorf("register coding agents: %w", err)
	}
	if len(enrolled.Added) > 0 {
		fmt.Fprintf(os.Stdout, "Registered %s with read-only permission. Widen it per agent in Settings when you need to.\n", strings.Join(enrolled.Added, ", "))
	} else if len(enrolled.Failed) == 0 {
		fmt.Fprintln(os.Stdout, "No new coding agents were registered. Sign in to Codex or Claude Code, then add them from Resources.")
	}
	if len(enrolled.Pending) > 0 {
		fmt.Fprintf(os.Stdout, "Found but not ready: %s.\n", strings.Join(enrolled.Pending, "; "))
	}
	if len(enrolled.Failed) > 0 {
		fmt.Fprintf(os.Stdout, "Could not register: %s. Run steve init again to retry them.\n", strings.Join(enrolled.Failed, "; "))
	}
	address, err := installed.AuthenticatedURL(result.URL)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "\nYou, me, and your friend Steve are ready.\nOpen %s\n", address)
	return nil
}

// shellQuote quotes a path for a POSIX shell.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
