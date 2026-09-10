package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/gopact-ai/steve/internal/plugins"
)

// PluginsCommand manages prepared packages without opening the live hub's
// configuration, ledger, harnesses or shared skills directories.
func PluginsCommand(args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runPluginsCommand(ctx, args, os.Stdout, os.Stderr)
}

type pluginFlags struct {
	source                   plugins.Source
	store, commandID, digest string
}

func runPluginsCommand(ctx context.Context, args []string, out, diagnostic io.Writer) error {
	if len(args) > 0 && (args[0] == "secret-put" || args[0] == "secret-list") {
		return runPluginSecretCommand(ctx, args, os.Stdin, out, diagnostic)
	}
	if len(args) == 0 {
		return errors.New("usage: steve plugins <preview|prepare|list|show|secret-put|secret-list> [flags]")
	}
	action := args[0]
	options, err := parsePluginFlags(action, args[1:], diagnostic)
	if err != nil {
		return err
	}
	var result any
	switch action {
	case "preview":
		bundle, source, err := plugins.Resolve(ctx, options.source)
		if err != nil {
			return err
		}
		result = struct {
			plugins.Bundle
			Source plugins.Source `json:"source"`
		}{Bundle: bundle, Source: source}
	case "prepare":
		store := plugins.Store{Dir: options.store}
		receipt, err := store.Prepare(ctx, options.commandID, options.digest, options.source)
		if err != nil {
			return err
		}
		result = receipt
	case "list":
		store := plugins.Store{Dir: options.store}
		receipts, err := store.List()
		if err != nil {
			return err
		}
		result = receipts
	case "show":
		store := plugins.Store{Dir: options.store}
		bundle, err := store.Read(options.digest)
		if err != nil {
			return err
		}
		result = bundle
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}

func parsePluginFlags(action string, args []string, out io.Writer) (pluginFlags, error) {
	var options pluginFlags
	flags := flag.NewFlagSet("plugins "+action, flag.ContinueOnError)
	flags.SetOutput(out)
	switch action {
	case "preview", "prepare":
		flags.StringVar(&options.source.Location, "source", "", "local package directory or HTTPS/local Git repository")
		flags.StringVar(&options.source.Commit, "commit", "", "full Git commit ID (selects Git source)")
		flags.StringVar(&options.source.Subdir, "subdir", "", "package directory within the Git commit")
	case "list", "show":
	default:
		return options, fmt.Errorf("unknown plugins command %q", action)
	}
	if action != "preview" {
		flags.StringVar(&options.store, "store", "", "explicit local package store directory")
	}
	if action == "prepare" || action == "show" {
		flags.StringVar(&options.digest, "digest", "", "exact SHA-256 digest returned by preview")
	}
	if action == "prepare" {
		flags.StringVar(&options.commandID, "command-id", "", "stable preparation command ID for retries")
	}
	if err := flags.Parse(args); err != nil {
		return options, err
	}
	if flags.NArg() != 0 {
		return options, errors.New("unexpected plugins command arguments")
	}
	if action != "preview" && options.store == "" {
		return options, errors.New("plugins command requires -store")
	}
	if action == "preview" || action == "prepare" {
		if options.source.Location == "" {
			return options, errors.New("plugins command requires -source")
		}
		options.source.Kind = "directory"
		if options.source.Commit != "" {
			options.source.Kind = "git"
		}
	}
	if action == "prepare" && options.commandID == "" {
		return options, errors.New("plugins prepare requires -command-id")
	}
	if (action == "prepare" || action == "show") && options.digest == "" {
		return options, fmt.Errorf("plugins %s requires -digest", action)
	}
	return options, nil
}
