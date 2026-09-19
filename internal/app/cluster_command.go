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

// InitClusterCommand prepares a stopped service for the normal `steve run`
// entry point. Only paths and public identity are printed, never credentials.
func InitClusterCommand(args []string) error {
	return runInitClusterCommand(args, os.Stdout, os.Stderr)
}

func runInitClusterCommand(args []string, out, diagnostic io.Writer) error {
	flags := flag.NewFlagSet("peer-init", flag.ContinueOnError)
	flags.SetOutput(diagnostic)
	options := cluster.ServiceBootstrap{}
	stateDir := flags.String("state-dir", "", "create a new installation in a private directory; cannot be combined with service configuration flags")
	flags.StringVar(&options.ConfigPath, "config", "config.json", "existing application configuration")
	flags.StringVar(&options.NodeID, "node-id", "", "stable peer node ID (generated if omitted)")
	flags.StringVar(&options.StorageLevel, "storage-level", "", "restricted or sealed; permits storing the private ledger")
	flags.StringVar(&options.RaftAddress, "raft-address", "", "reachable Raft host:port (default 127.0.0.1:0)")
	flags.StringVar(&options.PeerAddress, "peer-address", "", "reachable peer HTTPS host:port (default 127.0.0.1:0)")
	flags.StringVar(&options.UIAddress, "ui-address", "", "loopback console host:port (default configured read-model address)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected peer-init arguments")
	}
	fresh, service := false, false
	flags.Visit(func(option *flag.Flag) {
		if option.Name == "state-dir" {
			fresh = true
		} else {
			service = true
		}
	})
	if fresh {
		if service || strings.TrimSpace(*stateDir) == "" {
			return errors.New("peer-init --state-dir requires a nonempty directory and cannot be combined with existing-service options")
		}
		result, err := cluster.InitializePeer(*stateDir)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(result)
	}
	path, err := cluster.PrepareServiceCluster(options)
	if err != nil {
		return err
	}
	peer, err := cluster.LoadClusterPeerConfig(path)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(struct {
		Config  string `json:"cluster_config"`
		Cluster string `json:"cluster_id"`
		Node    string `json:"node_id"`
	}{path, peer.ClusterID, peer.NodeID})
}
