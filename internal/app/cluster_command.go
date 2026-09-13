package app

import (
	"encoding/json"
	"errors"
	"flag"
	"os"

	"github.com/gopact-ai/steve/internal/cluster"
)

// InitClusterCommand prepares a stopped service for the normal `steve run`
// entry point. Only paths and public identity are printed, never credentials.
func InitClusterCommand(args []string) error {
	flags := flag.NewFlagSet("peer-init", flag.ContinueOnError)
	options := cluster.ServiceBootstrap{}
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
	path, err := cluster.PrepareServiceCluster(options)
	if err != nil {
		return err
	}
	peer, err := cluster.LoadClusterPeerConfig(path)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(struct {
		Config  string `json:"cluster_config"`
		Cluster string `json:"cluster_id"`
		Node    string `json:"node_id"`
	}{path, peer.ClusterID, peer.NodeID})
}
