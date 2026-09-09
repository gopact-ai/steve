package app

import (
	"context"
	"errors"
	"flag"
	"os"

	"github.com/gopact-ai/steve/internal/node"
)

// MCPLaunchCommand lets a standalone hub's broker use the same secret-free
// binding launcher as steve-node without requiring another installed binary.
func MCPLaunchCommand(args []string) error {
	flags := flag.NewFlagSet(node.LaunchVerb, flag.ContinueOnError)
	socket := flags.String("socket", "", "local MCP broker socket")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *socket == "" || flags.NArg() != 1 {
		return errors.New("mcp-launch requires -socket and one binding ID")
	}
	return node.LaunchBinding(context.Background(), *socket, flags.Arg(0), os.Stdin, os.Stdout)
}
