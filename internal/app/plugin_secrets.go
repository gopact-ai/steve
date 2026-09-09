package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"strings"

	"github.com/gopact-ai/steve/internal/plugins"
)

// Secrets are configured by a local process on the node. The control plane
// receives only the returned reference, never the value supplied on stdin.
func runPluginSecretCommand(ctx context.Context, args []string, input io.Reader, out, diagnostic io.Writer) error {
	flags := flag.NewFlagSet("plugins "+args[0], flag.ContinueOnError)
	flags.SetOutput(diagnostic)
	dir := flags.String("store", "", "local package store on the executing node")
	name := ""
	if args[0] == "secret-put" {
		flags.StringVar(&name, "name", "", "node-local secret name")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if *dir == "" || flags.NArg() != 0 {
		return errors.New("secret command requires -store and accepts no positional arguments")
	}
	store := &plugins.Store{Dir: *dir}
	var result any
	switch args[0] {
	case "secret-put":
		raw, err := io.ReadAll(io.LimitReader(input, plugins.MaxSecretBytes+2))
		if err != nil {
			return err
		}
		value := strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
		info, err := store.PutSecret(ctx, name, value)
		if err != nil {
			return err
		}
		result = info
	case "secret-list":
		infos, err := store.Secrets()
		if err != nil {
			return err
		}
		result = infos
	default:
		return errors.New("unknown plugin secret command")
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}
