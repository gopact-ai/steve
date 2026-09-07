package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/desktop"
)

type clusterPeerConfig struct {
	Version          int                   `json:"version"`
	ClusterID        string                `json:"cluster_id"`
	NodeID           string                `json:"node_id"`
	FailureDomain    string                `json:"failure_domain,omitempty"`
	StorageLevel     string                `json:"storage_level"`
	Name             string                `json:"name"`
	Bootstrap        bool                  `json:"bootstrap"`
	DataDir          string                `json:"data_dir"`
	RaftAddress      string                `json:"raft_address"`
	PeerAddress      string                `json:"peer_address"`
	RaftBindAddress  string                `json:"raft_bind_address"`
	PeerBindAddress  string                `json:"peer_bind_address"`
	PeerURL          string                `json:"peer_url"`
	UIAddress        string                `json:"ui_address"`
	CACertFile       string                `json:"ca_cert_file"`
	CAKeyFile        string                `json:"ca_key_file,omitempty"`
	CertFile         string                `json:"cert_file"`
	KeyFile          string                `json:"key_file"`
	OwnerTokenFile   string                `json:"owner_token_file"`
	WorkerConfigFile string                `json:"worker_config_file"`
	Seeds            []coordination.Member `json:"seeds"`
}

func defaultClusterConfigPath(configPath string) string { return configPath + ".cluster.json" }

func loadClusterPeerConfig(path string) (clusterPeerConfig, error) {
	var config clusterPeerConfig
	data, err := readClusterPrivate(path)
	if err != nil {
		return config, err
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return config, fmt.Errorf("decode cluster configuration: %w", err)
	}
	if config.Version != 1 || config.ClusterID == "" || config.NodeID == "" || config.DataDir == "" {
		return config, errors.New("cluster configuration lacks a supported version or stable identity")
	}
	if config.StorageLevel != "restricted" && config.StorageLevel != "sealed" {
		return config, errors.New("完整机群节点必须明确授权保存 restricted 级别的私有协作账本")
	}
	for _, path := range []string{config.DataDir, config.CACertFile, config.CertFile, config.KeyFile, config.OwnerTokenFile} {
		if !filepath.IsAbs(path) {
			return config, errors.New("cluster state and certificate paths must be absolute")
		}
	}
	if config.WorkerConfigFile == "" {
		config.WorkerConfigFile = filepath.Join(config.DataDir, "node.json")
	}
	if _, _, err := net.SplitHostPort(config.RaftAddress); err != nil {
		return config, errors.New("invalid cluster Raft address")
	}
	if _, _, err := net.SplitHostPort(config.PeerAddress); err != nil {
		return config, errors.New("invalid cluster peer address")
	}
	if config.RaftBindAddress == "" {
		config.RaftBindAddress = config.RaftAddress
	}
	if config.PeerBindAddress == "" {
		config.PeerBindAddress = config.PeerAddress
	}
	if err := requireClusterLoopback(config.UIAddress); err != nil {
		return config, err
	}
	if config.PeerURL != "" {
		u, err := url.Parse(config.PeerURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return config, errors.New("cluster peer URL must be a plain HTTPS origin")
		}
	}
	return config, nil
}

func (c clusterPeerConfig) tlsOptions() (coordination.TLSOptions, error) {
	ca, err := readClusterPrivate(c.CACertFile)
	if err != nil {
		return coordination.TLSOptions{}, err
	}
	cert, err := readClusterPrivate(c.CertFile)
	if err != nil {
		return coordination.TLSOptions{}, err
	}
	key, err := readClusterPrivate(c.KeyFile)
	if err != nil {
		return coordination.TLSOptions{}, err
	}
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		return coordination.TLSOptions{}, fmt.Errorf("load node TLS identity: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return coordination.TLSOptions{}, errors.New("cluster CA file has no certificate")
	}
	options := coordination.TLSOptions{ClusterID: c.ClusterID, NodeID: c.NodeID, Certificate: pair, RootCAs: roots}
	if _, err := options.ServerConfig(); err != nil {
		return coordination.TLSOptions{}, err
	}
	return options, nil
}

func prepareDesktopCluster(configPath string) (string, error) {
	path := defaultClusterConfigPath(configPath)
	if _, err := os.Lstat(path); err == nil {
		return path, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	installed, err := desktop.Bootstrap(desktop.Options{StateDir: filepath.Dir(configPath)})
	if err != nil {
		return "", err
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return "", err
	}
	id, err := clusterRandomID("cluster-")
	if err != nil {
		return "", err
	}
	dir := filepath.Join(installed.Paths.Root, "cluster")
	if _, err := os.Stat(dir); err == nil {
		if _, err := os.Stat(filepath.Join(dir, "raft")); err == nil {
			return "", errors.New("cluster configuration is missing for an initialized peer")
		}
		saved, err := loadClusterPeerConfig(filepath.Join(dir, "bootstrap.json"))
		if err != nil {
			return "", fmt.Errorf("recover cluster initialization: %w", err)
		}
		if saved.NodeID != installed.NodeID || saved.DataDir != dir {
			return "", errors.New("cluster initialization identity differs from the desktop")
		}
		if _, err := saved.tlsOptions(); err != nil {
			return "", err
		}
		if _, err := readClusterPrivate(saved.OwnerTokenFile); err != nil {
			return "", err
		}
		return path, saveClusterJSON(path, saved, true)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	name, err := os.Hostname()
	if err != nil {
		name = installed.NodeID
	}
	peer := clusterPeerConfig{Version: 1, ClusterID: id, NodeID: installed.NodeID, StorageLevel: "restricted", Name: name, Bootstrap: true, DataDir: dir, RaftAddress: "127.0.0.1:0", PeerAddress: "127.0.0.1:0", UIAddress: cfg.Gateway.ReadModelAddr, CACertFile: filepath.Join(dir, "ca.pem"), CAKeyFile: filepath.Join(dir, "ca-key.pem"), CertFile: filepath.Join(dir, "node.pem"), KeyFile: filepath.Join(dir, "node-key.pem"), OwnerTokenFile: filepath.Join(dir, "owner-control-token")}
	peer.RaftBindAddress = "0.0.0.0:0"
	peer.PeerBindAddress = "0.0.0.0:0"
	if addresses := localAdvertiseAddresses(); len(addresses) > 0 {
		peer.RaftAddress = net.JoinHostPort(addresses[0], "0")
		peer.PeerAddress = net.JoinHostPort(addresses[0], "0")
	}
	peer.WorkerConfigFile = filepath.Join(dir, "node.json")
	staging, err := os.MkdirTemp(installed.Paths.Root, ".cluster-init-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staging)
	temporary := peer
	temporary.CACertFile = filepath.Join(staging, "ca.pem")
	temporary.CAKeyFile = filepath.Join(staging, "ca-key.pem")
	temporary.CertFile = filepath.Join(staging, "node.pem")
	temporary.KeyFile = filepath.Join(staging, "node-key.pem")
	temporary.OwnerTokenFile = filepath.Join(staging, "owner-control-token")
	if err := createClusterAuthority(temporary); err != nil {
		return "", err
	}
	if err := saveClusterJSON(filepath.Join(staging, "bootstrap.json"), peer, true); err != nil {
		return "", err
	}
	if err := os.Rename(staging, dir); err != nil {
		return "", err
	}
	parent, err := os.Open(installed.Paths.Root)
	if err != nil {
		return "", err
	}
	err = parent.Sync()
	parent.Close()
	if err != nil {
		return "", err
	}
	if err := saveClusterJSON(path, peer, true); err != nil {
		return "", err
	}
	return path, nil
}

func createClusterAuthority(c clusterPeerConfig) error {
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return err
	}
	certificate := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: c.ClusterID}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().AddDate(10, 0, 0), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, public, key)
	if err != nil {
		return err
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		return err
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	leaf, leafKey, err := issueClusterNodeCertificate(parsed, key, c.ClusterID, c.NodeID)
	if err != nil {
		return err
	}
	token, err := clusterRandomToken()
	if err != nil {
		return err
	}
	for _, file := range []struct {
		path string
		data []byte
	}{{c.CACertFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}, {c.CAKeyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private})}, {c.CertFile, leaf}, {c.KeyFile, leafKey}, {c.OwnerTokenFile, []byte(token)}} {
		if err := writeClusterPrivate(file.path, file.data, true); err != nil {
			return err
		}
	}
	return nil
}

func issueClusterNodeCertificate(ca *x509.Certificate, caKey ed25519.PrivateKey, clusterID, nodeID string) ([]byte, []byte, error) {
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return nil, nil, err
	}
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: nodeID}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().AddDate(1, 0, 0), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}, URIs: []*url.URL{coordination.IdentityURI(clusterID, nodeID)}}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, public, caKey)
	if err != nil {
		return nil, nil, err
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}), nil
}

func clusterRandomID(prefix string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b[:]), nil
}
func clusterRandomToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// Raw machine identifiers never enter configuration, logs or network replies.
func physicalFailureDomain() (string, error) {
	var raw string
	switch runtime.GOOS {
	case "linux":
		data, err := os.ReadFile("/etc/machine-id")
		if err != nil {
			return "", err
		}
		raw = strings.TrimSpace(string(data))
		if !regexp.MustCompile(`^[[:xdigit:]]{32}$`).MatchString(raw) {
			return "", errors.New("machine identity is unavailable")
		}
	case "darwin":
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		data, err := exec.CommandContext(ctx, "/usr/sbin/ioreg", "-rd1", "-c", "IOPlatformExpertDevice").Output()
		if err != nil {
			return "", errors.New("machine identity is unavailable")
		}
		match := regexp.MustCompile(`"IOPlatformUUID"\s*=\s*"([[:xdigit:]-]{36})"`).FindSubmatch(data)
		if len(match) != 2 {
			return "", errors.New("machine identity is unavailable")
		}
		raw = string(match[1])
	default:
		return "", errors.New("machine identity is not supported on this platform")
	}
	digest := sha256.Sum256([]byte("steve/failure-domain/v1/" + runtime.GOOS + "/" + strings.ToLower(raw)))
	return "machine-" + hex.EncodeToString(digest[:]), nil
}

func requireClusterLoopback(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New("cluster UI needs an explicit loopback address")
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("cluster UI gateway must bind to loopback")
	}
	return nil
}

func readClusterPrivate(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > 8<<20 {
		return nil, fmt.Errorf("%s must be a private regular file within the size limit", filepath.Base(path))
	}
	return os.ReadFile(path)
}

func saveClusterJSON(path string, value any, create bool) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writeClusterPrivate(path, append(data, '\n'), create)
}

func writeClusterPrivate(path string, data []byte, create bool) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("cluster file path is empty")
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".cluster-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if create {
		err = os.Link(file.Name(), path)
	} else {
		err = os.Rename(file.Name(), path)
	}
	if err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
