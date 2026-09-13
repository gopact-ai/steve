package cluster

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
