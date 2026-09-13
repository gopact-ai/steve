package cluster

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/hubid"
	steveruntime "github.com/gopact-ai/steve/internal/runtime"
)

// ServiceBootstrap initializes a service peer without a desktop profile. The
// existing application configuration, owner, ledger and hub identity stay put.
type ServiceBootstrap struct {
	ConfigPath   string
	NodeID       string
	StorageLevel string
	RaftAddress  string
	PeerAddress  string
	UIAddress    string
}

var serviceNodeID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

func PrepareServiceCluster(options ServiceBootstrap) (string, error) {
	configPath, err := filepath.Abs(options.ConfigPath)
	if err != nil {
		return "", err
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return "", err
	}
	if err := validateServiceBootstrap(options, cfg); err != nil {
		return "", err
	}
	root := filepath.Dir(cfg.Gateway.StatePath)
	unlock, err := steveruntime.AcquireLock(root)
	if err != nil {
		return "", fmt.Errorf("stop the application before initializing its cluster: %w", err)
	}
	defer unlock()
	identity, err := hubid.Resolve(root, cfg.Gateway.HubID)
	if err != nil {
		return "", err
	}
	path := DefaultClusterConfigPath(configPath)
	dir := filepath.Join(root, "cluster")
	// A follower can be alive without the application holding the state lock.
	// Check its process lock too before recovering or validating bootstrap.
	if _, err := os.Stat(dir); err == nil {
		unlockPeer, err := steveruntime.AcquireLock(filepath.Join(dir, "peer-process"))
		if err != nil {
			return "", fmt.Errorf("stop the peer before initializing its cluster: %w", err)
		}
		defer unlockPeer()
	}
	if _, err := os.Lstat(path); err == nil {
		_, err := validateServicePeer(path, dir, identity, options)
		return path, err
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if _, err := os.Lstat(dir); err == nil {
		if _, err := os.Lstat(filepath.Join(dir, "raft")); !errors.Is(err, os.ErrNotExist) {
			return "", errors.New("cluster sidecar is missing for an initialized peer; restore its original configuration")
		}
		saved, err := validateServicePeer(filepath.Join(dir, "bootstrap.json"), dir, identity, options)
		if err != nil {
			return "", fmt.Errorf("recover service cluster initialization: %w", err)
		}
		return path, SaveClusterJSON(path, saved, true)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	nodeID := options.NodeID
	if nodeID == "" {
		nodeID, err = clusterRandomID("node-")
		if err != nil {
			return "", err
		}
	}
	ui := options.UIAddress
	if ui == "" {
		ui = cfg.Gateway.ReadModelAddr
	}
	name, err := os.Hostname()
	if err != nil {
		name = nodeID
	}
	peer := PeerConfig{Version: 1, ClusterID: identity, NodeID: nodeID, Name: name, Bootstrap: true,
		StorageLevel: options.StorageLevel, DataDir: dir, RaftAddress: serviceAddress(options.RaftAddress),
		PeerAddress: serviceAddress(options.PeerAddress), UIAddress: ui,
		CACertFile: filepath.Join(dir, "ca.pem"), CAKeyFile: filepath.Join(dir, "ca-key.pem"),
		CertFile: filepath.Join(dir, "node.pem"), KeyFile: filepath.Join(dir, "node-key.pem"),
		OwnerTokenFile: filepath.Join(dir, "owner-control-token"), WorkerConfigFile: filepath.Join(dir, "node.json")}
	peer.RaftBindAddress, peer.PeerBindAddress = peer.RaftAddress, peer.PeerAddress
	if err := publishClusterBootstrap(root, path, peer); err != nil {
		return "", err
	}
	return path, nil
}

func serviceAddress(address string) string {
	if address == "" {
		return "127.0.0.1:0"
	}
	return address
}

func validateServiceBootstrap(options ServiceBootstrap, cfg *config.Config) error {
	if options.StorageLevel != "restricted" && options.StorageLevel != "sealed" {
		return errors.New("peer-init requires --storage-level restricted or sealed for the private collaboration ledger")
	}
	if len(cfg.Gateway.ReadModelToken) < 32 {
		return errors.New("gateway.read_model_token must contain at least 32 characters before peer-init")
	}
	if options.NodeID != "" && !serviceNodeID.MatchString(options.NodeID) {
		return errors.New("invalid peer node ID")
	}
	for _, address := range []string{serviceAddress(options.RaftAddress), serviceAddress(options.PeerAddress)} {
		host, _, err := net.SplitHostPort(address)
		if err != nil || host == "" || host == "0.0.0.0" || host == "::" {
			return errors.New("peer addresses need a concrete reachable host and port")
		}
		if _, err := net.ResolveTCPAddr("tcp", address); err != nil {
			return fmt.Errorf("invalid peer address: %w", err)
		}
	}
	ui := options.UIAddress
	if ui == "" {
		ui = cfg.Gateway.ReadModelAddr
	}
	return requireClusterLoopback(ui)
}

func validateServicePeer(path, dir, identity string, options ServiceBootstrap) (PeerConfig, error) {
	saved, err := LoadClusterPeerConfig(path)
	if err != nil {
		return saved, err
	}
	if saved.ClusterID != identity || saved.DataDir != dir || (options.NodeID != "" && saved.NodeID != options.NodeID) || saved.StorageLevel != options.StorageLevel {
		return saved, errors.New("existing cluster initialization differs from the requested service identity")
	}
	for _, pair := range [][2]string{{options.RaftAddress, saved.RaftAddress}, {options.PeerAddress, saved.PeerAddress}, {options.UIAddress, saved.UIAddress}} {
		if pair[0] != "" && pair[0] != pair[1] {
			return saved, errors.New("peer-init cannot change an existing peer's addresses")
		}
	}
	if _, err := saved.TlsOptions(); err != nil {
		return saved, err
	}
	token, err := ReadClusterPrivate(saved.OwnerTokenFile)
	if err == nil && (len(token) < 32 || strings.ContainsAny(string(token), "\r\n\t ")) {
		err = errors.New("invalid cluster owner token")
	}
	return saved, err
}

func publishClusterBootstrap(root, path string, peer PeerConfig) error {
	staging, err := os.MkdirTemp(root, ".cluster-init-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	temporary := peer
	temporary.CACertFile = filepath.Join(staging, "ca.pem")
	temporary.CAKeyFile = filepath.Join(staging, "ca-key.pem")
	temporary.CertFile = filepath.Join(staging, "node.pem")
	temporary.KeyFile = filepath.Join(staging, "node-key.pem")
	temporary.OwnerTokenFile = filepath.Join(staging, "owner-control-token")
	if err := createClusterAuthority(temporary); err != nil {
		return err
	}
	if err := SaveClusterJSON(filepath.Join(staging, "bootstrap.json"), peer, true); err != nil {
		return err
	}
	if err := os.Rename(staging, peer.DataDir); err != nil {
		return err
	}
	parent, err := os.Open(root)
	if err != nil {
		return err
	}
	syncErr := parent.Sync()
	closeErr := parent.Close()
	if err := errors.Join(syncErr, closeErr); err != nil {
		return err
	}
	return SaveClusterJSON(path, peer, true)
}
