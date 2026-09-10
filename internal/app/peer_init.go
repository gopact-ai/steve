package app

import (
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"strings"

	"github.com/gopact-ai/steve/internal/cluster"
)

// PeerInitCommand creates a cluster installation without starting its service.
func PeerInitCommand(args []string) error {
	return runPeerInitCommand(args, os.Stdout, os.Stderr)
}

func runPeerInitCommand(args []string, out, diagnostic io.Writer) error {
	flags := flag.NewFlagSet("peer-init", flag.ContinueOnError)
	flags.SetOutput(diagnostic)
	stateDir := flags.String("state-dir", "", "private directory for a new cluster, its authority and restricted collaboration ledger (required)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*stateDir) == "" || flags.NArg() != 0 {
		return errors.New("usage: steve peer-init --state-dir <new-private-directory>")
	}
	result, err := cluster.InitializePeer(*stateDir)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(result)
}
