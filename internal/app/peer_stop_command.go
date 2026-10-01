package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/gopact-ai/steve/internal/peerstop"
)

// The protocol probe is a flag so an older binary rejects it before it could
// interpret an unknown subcommand as a request to serve an application.
const peerStopProtocol = "STEVE_PEER_STOP_V1"

func PeerStopCommand(args []string) error {
	flags := flag.NewFlagSet("peer-stop", flag.ContinueOnError)
	sidecar := flags.String("cluster-config", "", "identified installation sidecar")
	cluster := flags.String("expect-cluster", "", "expected cluster identity")
	node := flags.String("expect-node", "", "expected node identity")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *sidecar == "" || *cluster == "" || *node == "" {
		return errors.New("peer-stop requires an installation and its expected cluster and node")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	running, err := peerstop.Stop(ctx, *sidecar, *cluster, *node)
	outcome := "absent"
	switch {
	case errors.Is(err, peerstop.ErrUnsupported):
		outcome = "unsupported"
	case err != nil:
		outcome = "unproven"
	case running:
		outcome = "stopped"
	}
	fmt.Fprintln(os.Stdout, "STEVE_PEER_STOP\t"+outcome)
	return err
}

// PeerStopProtocolCommand is a read-only capability query, including on a
// machine whose peer cannot answer its cluster API.
func PeerStopProtocolCommand(args []string) error {
	if len(args) != 0 {
		return errors.New("unexpected peer-stop protocol arguments")
	}
	_, err := fmt.Fprintln(os.Stdout, peerStopProtocol)
	return err
}
