package cluster

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gopact-ai/steve/internal/desktop"
	runtimestore "github.com/gopact-ai/steve/internal/runtime"
)

// PeerInitialization contains public identifiers and local paths, never secrets.
type PeerInitialization struct {
	Config        string `json:"config"`
	ClusterConfig string `json:"cluster_config"`
	ClusterID     string `json:"cluster_id"`
	NodeID        string `json:"node_id"`
	TokenFile     string `json:"token_file"`
	EndpointFile  string `json:"endpoint_file"`
}

// InitializePeer prepares the first peer for service deployment. It shares the
// desktop installation format, including stable identity and endpoint discovery,
// but does not start a process or import another installation's state.
func InitializePeer(stateDir string) (PeerInitialization, error) {
	var result PeerInitialization
	if strings.TrimSpace(stateDir) == "" {
		return result, errors.New("peer initialization requires a state directory")
	}
	root, err := filepath.Abs(stateDir)
	if err != nil {
		return result, err
	}
	if err := preparePeerDirectory(root); err != nil {
		return result, err
	}
	// Share the application's lock: reject concurrent initialization or a live
	// application rather than changing its files while it is running.
	unlock, err := runtimestore.AcquireLock(root)
	if err != nil {
		return result, err
	}
	defer unlock()
	unlockPeer, err := lockExistingPeer(root)
	if err != nil {
		return result, err
	}
	defer unlockPeer()
	installed, err := desktop.Bootstrap(desktop.Options{StateDir: root})
	if err != nil {
		return result, err
	}
	if err := validatePeerRecovery(installed); err != nil {
		return result, err
	}
	path, err := PrepareDesktopCluster(installed.Paths.Config)
	if err != nil {
		return result, err
	}
	peer, err := LoadClusterPeerConfig(path)
	if err != nil {
		return result, err
	}
	if err := validateInitializedPeer(peer, installed); err != nil {
		return result, err
	}
	return PeerInitialization{Config: installed.Paths.Config, ClusterConfig: path, ClusterID: peer.ClusterID,
		NodeID: peer.NodeID, TokenFile: installed.Paths.Token, EndpointFile: installed.Paths.Endpoint}, nil
}

func lockExistingPeer(root string) (func(), error) {
	dir := filepath.Join(root, "cluster")
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return func() {}, nil
	}
	if err != nil {
		return nil, err
	}
	if err := checkPrivateDirectory(dir, info); err != nil {
		return nil, err
	}
	// Authority is only published after these files exist. Missing credentials
	// in an established installation must not be silently regenerated.
	for _, name := range []string{"config.json", "node-identity.json", "loopback-token"} {
		if _, err := ReadClusterPrivate(filepath.Join(root, name)); err != nil {
			return nil, err
		}
	}
	process := filepath.Join(dir, "peer-process")
	if info, err := os.Lstat(process); err == nil {
		if err := checkPrivateDirectory(process, info); err != nil {
			return nil, err
		}
		if _, err := ReadClusterPrivate(filepath.Join(process, "gateway.lock")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return runtimestore.AcquireLock(process)
}

func validatePeerRecovery(installed *desktop.Installation) error {
	path := DefaultClusterConfigPath(installed.Paths.Config)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		dir := filepath.Join(installed.Paths.Root, "cluster")
		if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
			return nil
		}
		path = filepath.Join(dir, "bootstrap.json")
	}
	peer, err := LoadClusterPeerConfig(path)
	if err != nil {
		return err
	}
	return validateInitializedPeer(peer, installed)
}

func preparePeerDirectory(root string) error {
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return os.MkdirAll(root, 0o700)
	}
	if err != nil {
		return err
	}
	if err := checkPrivateDirectory(root, info); err != nil {
		return err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	managed := desktop.IsManagedConfig(filepath.Join(root, "config.json"))
	for _, entry := range entries {
		if entry.Name() == "gateway.lock" || entry.Name() == ".desktop-init.lock" {
			if _, err := ReadClusterPrivate(filepath.Join(root, entry.Name())); err != nil {
				return err
			}
			continue
		}
		if !managed {
			return errors.New("peer state directory contains an existing installation or unknown files; choose a new directory")
		}
	}
	return nil
}

func validateInitializedPeer(peer PeerConfig, installed *desktop.Installation) error {
	dir := filepath.Join(installed.Paths.Root, "cluster")
	if peer.NodeID != installed.NodeID || peer.DataDir != dir || peer.UIAddress != strings.TrimPrefix(installed.URL, "http://") || !peer.Bootstrap {
		return errors.New("cluster initialization does not match this installation's identity, state directory or listener")
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if err := checkPrivateDirectory(dir, info); err != nil {
		return err
	}
	for _, file := range []struct{ path, name string }{
		{peer.CACertFile, "ca.pem"}, {peer.CAKeyFile, "ca-key.pem"}, {peer.CertFile, "node.pem"},
		{peer.KeyFile, "node-key.pem"}, {peer.OwnerTokenFile, "owner-control-token"}, {peer.WorkerConfigFile, "node.json"},
	} {
		if file.path != filepath.Join(dir, file.name) {
			return fmt.Errorf("cluster initialization has an unexpected %s path", file.name)
		}
	}
	if err := validateBootstrapAuthority(peer); err != nil {
		return err
	}
	owner, err := ReadClusterPrivate(peer.OwnerTokenFile)
	if err != nil {
		return err
	}
	if len(owner) < 32 || strings.ContainsAny(string(owner), "\r\n\t ") {
		return errors.New("cluster owner token is invalid")
	}
	return nil
}

func checkPrivateDirectory(path string, info os.FileInfo) error {
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("peer initialization directory %s is a symbolic link", path)
	case !info.IsDir():
		return fmt.Errorf("peer initialization path %s is not a directory", path)
	case info.Mode().Perm()&0o077 != 0:
		return fmt.Errorf("peer initialization directory %s must be private to its owner; permissions are %#o", path, info.Mode().Perm())
	}
	return nil
}

func validateBootstrapAuthority(peer PeerConfig) error {
	identity, err := peer.TlsOptions()
	if err != nil {
		return err
	}
	ca, err := ReadClusterPrivate(peer.CACertFile)
	if err != nil {
		return err
	}
	key, err := ReadClusterPrivate(peer.CAKeyFile)
	if err != nil {
		return err
	}
	if _, err := tls.X509KeyPair(ca, key); err != nil {
		return fmt.Errorf("invalid cluster signing authority: %w", err)
	}
	leaf, err := x509.ParseCertificate(identity.Certificate.Certificate[0])
	if err != nil {
		return err
	}
	for _, usage := range []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth} {
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: identity.RootCAs, KeyUsages: []x509.ExtKeyUsage{usage}}); err != nil {
			return fmt.Errorf("verify initialized peer certificate: %w", err)
		}
	}
	return nil
}
