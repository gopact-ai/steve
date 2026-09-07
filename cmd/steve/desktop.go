package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/gopact-ai/steve/internal/desktop"
)

// desktopCmd is a short-lived launcher. The service it starts is detached and
// remains available when a window or the native application closes.
func desktopCmd(args []string) error {
	flags := flag.NewFlagSet("desktop", flag.ContinueOnError)
	stateDir := flags.String("state-dir", "", "private desktop data directory")
	jsonOutput := flags.Bool("json", false, "return an authenticated local URL for a native launcher")
	timeout := flags.Duration("timeout", 30*time.Second, "maximum time to wait for the local service")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected desktop arguments")
	}
	installed, err := desktop.Bootstrap(desktop.Options{StateDir: *stateDir})
	if err != nil {
		return err
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
	if *jsonOutput {
		authenticated, err := installed.AuthenticatedURL(result.URL)
		if err != nil {
			return err
		}
		// stdout is a private pipe to the launcher, never a log stream.
		return json.NewEncoder(os.Stdout).Encode(struct {
			desktop.LaunchResult
			AuthenticatedURL string `json:"authenticated_url"`
		}{LaunchResult: result, AuthenticatedURL: authenticated})
	}
	fmt.Fprintf(os.Stdout, "Steve is available at %s (pid %d). Open the Steve app to connect.\n", result.URL, result.PID)
	return nil
}
