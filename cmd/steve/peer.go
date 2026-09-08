package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/gopact-ai/steve/internal/app"
	"github.com/gopact-ai/steve/internal/cluster"
	"github.com/gopact-ai/steve/internal/desktop"
)

func peerImportCmd(args []string) error {
	flags := flag.NewFlagSet("peer-import", flag.ContinueOnError)
	packagePath := flags.String("package", "", "private enrollment package")
	stateDir := flags.String("state-dir", "", "new peer state directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *packagePath == "" || *stateDir == "" || flags.NArg() != 0 {
		return errors.New("peer-import requires --package and --state-dir")
	}
	data, err := cluster.ReadClusterPrivate(*packagePath)
	if err != nil {
		return err
	}
	result, err := cluster.ImportPeerPackage(data, *stateDir)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}

func maybeManagedPeer(args []string) (bool, error) {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "config.json", "configuration")
	if err := flags.Parse(args); err != nil {
		return false, nil
	}
	path := cluster.DefaultClusterConfigPath(*configPath)
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if !desktop.IsManagedConfig(*configPath) {
			return false, nil
		}
		path, err = cluster.PrepareDesktopCluster(*configPath)
	}
	if err != nil {
		return true, err
	}
	return true, peerCmd([]string{"--config", *configPath, "--cluster-config", path})
}

func peerCmd(args []string) error {
	flags := flag.NewFlagSet("peer", flag.ContinueOnError)
	configPath := flags.String("config", "config.json", "application configuration")
	clusterPath := flags.String("cluster-config", "", "private cluster configuration sidecar")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected peer arguments")
	}
	if *clusterPath == "" {
		*clusterPath = cluster.DefaultClusterConfigPath(*configPath)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	peer, err := app.OpenClusterPeer(ctx, cluster.PeerOptions{ConfigPath: *configPath, ClusterPath: *clusterPath, AllowAutoFailover: true})
	if err != nil {
		return err
	}
	log.Printf("steve: cluster peer %s UI available at %s", peer.Config.NodeID, peer.UiURL)
	select {
	case <-ctx.Done():
	case err = <-peer.Errors:
	}
	return errors.Join(err, peer.Close())
}
