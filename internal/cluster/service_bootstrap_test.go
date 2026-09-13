package cluster

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/hubid"
	steveruntime "github.com/gopact-ai/steve/internal/runtime"
)

func serviceFixture(t *testing.T) (ServiceBootstrap, string) {
	t.Helper()
	root := t.TempDir()
	cfg := filepath.Join(root, "service.json")
	state := filepath.Join(root, "existing-state")
	raw, err := json.Marshal(map[string]any{"gateway": map[string]any{"hub_id": "existing-hub", "owner_id": "existing-owner", "state_path": filepath.Join(state, "state.json"), "home_path": filepath.Join(state, "home"), "read_model_addr": "127.0.0.1:0", "read_model_token": strings.Repeat("token", 8)}, "projects": map[string]any{"scratch": map[string]any{"home": map[string]any{"path": filepath.Join(root, "scratch")}}}, "agents": map[string]any{"worker": map[string]any{"harness": "mock", "default": true}}, "harnesses": map[string]any{"mock": map[string]any{"command": "/bin/false", "permission": "read"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return ServiceBootstrap{ConfigPath: cfg, StorageLevel: "restricted"}, state
}

func TestServiceBootstrapPreservesStandaloneIdentityConfigurationAndData(t *testing.T) {
	options, root := serviceFixture(t)
	id, err := hubid.Resolve(root, "existing-hub")
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(options.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(root, "operator-data")
	if err := os.WriteFile(sentinel, []byte("existing state"), 0600); err != nil {
		t.Fatal(err)
	}
	path, err := PrepareServiceCluster(options)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := LoadClusterPeerConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if peer.ClusterID != id || peer.DataDir != filepath.Join(root, "cluster") || peer.NodeID == "" {
		t.Fatalf("peer identity = %+v", peer)
	}
	after, _ := os.ReadFile(options.ConfigPath)
	data, _ := os.ReadFile(sentinel)
	if !bytes.Equal(original, after) || string(data) != "existing state" {
		t.Fatal("bootstrap rewrote application data or config")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(options.ConfigPath), "desktop.json")); !os.IsNotExist(err) {
		t.Fatal("service bootstrap created a desktop profile")
	}
	if _, err := peer.TlsOptions(); err != nil {
		t.Fatal(err)
	}
	again, err := PrepareServiceCluster(options)
	if err != nil || again != path {
		t.Fatalf("idempotent bootstrap: %s %v", again, err)
	}
	reloaded, _ := LoadClusterPeerConfig(again)
	if reloaded.NodeID != peer.NodeID {
		t.Fatal("idempotent bootstrap reminted node")
	}
	unlock, err := steveruntime.AcquireLock(filepath.Join(peer.DataDir, "peer-process"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareServiceCluster(options); err == nil {
		t.Fatal("initialized while a follower peer was alive")
	}
	unlock()
	options.NodeID = "another-node"
	if _, err := PrepareServiceCluster(options); err == nil {
		t.Fatal("bootstrap changed identity")
	}
}

func TestServiceBootstrapRecoveryReusesPublishedAuthority(t *testing.T) {
	options, _ := serviceFixture(t)
	path, err := PrepareServiceCluster(options)
	if err != nil {
		t.Fatal(err)
	}
	before, err := LoadClusterPeerConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := os.ReadFile(before.CertFile)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareServiceCluster(options); err != nil {
		t.Fatal(err)
	}
	after, err := LoadClusterPeerConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	restored, _ := os.ReadFile(after.CertFile)
	if before.NodeID != after.NodeID || before.ClusterID != after.ClusterID || !bytes.Equal(cert, restored) {
		t.Fatal("recovery changed authority")
	}
	if err := os.Mkdir(filepath.Join(before.DataDir, "raft"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareServiceCluster(options); err == nil {
		t.Fatal("initialized peer lost its bound configuration but was reinitialized")
	}
}

func TestServiceBootstrapRequiresStoppedServiceAndValidInputs(t *testing.T) {
	options, root := serviceFixture(t)
	unlock, err := steveruntime.AcquireLock(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareServiceCluster(options); err == nil {
		t.Fatal("initialized live application")
	}
	unlock()
	for _, mutate := range []func(*ServiceBootstrap){
		func(o *ServiceBootstrap) { o.StorageLevel = "public" },
		func(o *ServiceBootstrap) { o.UIAddress = "0.0.0.0:7710" },
		func(o *ServiceBootstrap) { o.UIAddress = "127.0.0.1:99999" },
		func(o *ServiceBootstrap) { o.UIAddress = "127.0.0.1:http" },
		func(o *ServiceBootstrap) { o.RaftAddress = "0.0.0.0:7801" },
		func(o *ServiceBootstrap) { o.NodeID = "../other" },
		func(o *ServiceBootstrap) { o.NodeID = "node?unexpected" },
	} {
		invalid := options
		mutate(&invalid)
		if _, err := PrepareServiceCluster(invalid); err == nil {
			t.Fatalf("accepted invalid options: %+v", invalid)
		}
	}
	if _, err := os.Stat(DefaultClusterConfigPath(options.ConfigPath)); !os.IsNotExist(err) {
		t.Fatal("invalid initialization published sidecar")
	}
}

func TestServiceBootstrapKeepsAllocatedPortsOnExplicitZeroRetry(t *testing.T) {
	options, _ := serviceFixture(t)
	options.RaftAddress, options.PeerAddress, options.UIAddress = "127.0.0.1:0", "127.0.0.1:0", "127.0.0.1:0"
	path, err := PrepareServiceCluster(options)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := LoadClusterPeerConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	peer.RaftAddress, peer.PeerAddress, peer.UIAddress = "127.0.0.1:18001", "127.0.0.1:18002", "127.0.0.1:18003"
	if err := SaveClusterJSON(path, peer, false); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareServiceCluster(options); err != nil {
		t.Fatal(err)
	}
	got, err := LoadClusterPeerConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.NodeID != peer.NodeID || got.RaftAddress != peer.RaftAddress || got.UIAddress != peer.UIAddress {
		t.Fatal("retry replaced allocated addresses or identity")
	}
	options.PeerAddress = "127.0.0.2:0"
	if _, err := PrepareServiceCluster(options); err == nil {
		t.Fatal("retry accepted a different host")
	}
}

func TestServiceBootstrapRejectsUnusableUITokensBeforePublishing(t *testing.T) {
	for _, whitespace := range []string{" ", "\t", "\r", "\n"} {
		options, _ := serviceFixture(t)
		raw, err := os.ReadFile(options.ConfigPath)
		if err != nil {
			t.Fatal(err)
		}
		var cfg map[string]any
		if err := json.Unmarshal(raw, &cfg); err != nil {
			t.Fatal(err)
		}
		cfg["gateway"].(map[string]any)["read_model_token"] = strings.Repeat("a", 32) + whitespace + "z"
		raw, err = json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(options.ConfigPath, raw, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := PrepareServiceCluster(options); err == nil {
			t.Fatal("accepted token containing whitespace")
		}
		if _, err := os.Stat(DefaultClusterConfigPath(options.ConfigPath)); !os.IsNotExist(err) {
			t.Fatal("published unusable sidecar")
		}
	}
}

func TestServiceBootstrapRejectsConflictingListenersBeforePublication(t *testing.T) {
	for _, addresses := range [][3]string{{"127.0.0.1:7801", "127.0.0.1:7801", "127.0.0.1:0"}, {"127.0.0.1:7801", "127.0.0.1:0", "127.0.0.1:7801"}, {"127.0.0.1:0", "127.0.0.1:7801", "127.0.0.1:7801"}} {
		options, _ := serviceFixture(t)
		options.RaftAddress, options.PeerAddress, options.UIAddress = addresses[0], addresses[1], addresses[2]
		if _, err := PrepareServiceCluster(options); err == nil {
			t.Fatalf("accepted conflicting listeners: %v", addresses)
		}
		if _, err := os.Stat(DefaultClusterConfigPath(options.ConfigPath)); !os.IsNotExist(err) {
			t.Fatal("published conflicting listeners")
		}
	}
	if err := distinctServiceEndpoints("127.0.0.1:0", "127.0.0.1:0", "127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
}

func TestServiceBootstrapReloadsLockedConfigurationAndPreservesCapabilities(t *testing.T) {
	options, root := serviceFixture(t)
	cfg, err := config.Load(options.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := steveruntime.AcquireLock(root)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	cfg.Gateway.Tools = []string{"go"}
	cfg.Gateway.Capabilities = []string{"build"}
	cfg.Gateway.Declares = []string{"filesystem:repo"}
	cfg.Harnesses["mock"] = config.Harness{Command: "/bin/true", Permission: "auto"}
	if err := config.Save(options.ConfigPath, cfg); err != nil {
		t.Fatal(err)
	}
	refreshed, err := loadLockedServiceConfig(options.ConfigPath, root, options)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.Harnesses["mock"].Command != "/bin/true" {
		t.Fatal("used the config read before locking")
	}
	worker, err := serviceWorker(PeerConfig{NodeID: "worker", ClusterID: "cluster", DataDir: filepath.Join(root, "cluster")}, refreshed)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(worker.Tools, cfg.Gateway.Tools) || !slices.Equal(worker.Capabilities, cfg.Gateway.Capabilities) || !slices.Equal(worker.Declares, cfg.Gateway.Declares) {
		t.Fatal("worker lost physical capability declarations")
	}
	refreshed.Gateway.Tools[0] = "changed"
	if worker.Tools[0] != "go" {
		t.Fatal("worker aliased application capabilities")
	}
	cfg.Gateway.StatePath = filepath.Join(root, "moved", "state.json")
	if err := config.Save(options.ConfigPath, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := loadLockedServiceConfig(options.ConfigPath, root, options); err == nil {
		t.Fatal("used a configuration whose state was not locked")
	}
}
