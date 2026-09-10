package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gopact-ai/steve/internal/cluster"
)

func TestPeerInitCreatesConfigurationWithoutStartingService(t *testing.T) {
	root := filepath.Join(t.TempDir(), "service")
	if err := run([]string{"peer-init", "--state-dir", root}); err != nil {
		t.Fatal(err)
	}
	path := cluster.DefaultClusterConfigPath(filepath.Join(root, "config.json"))
	before, err := cluster.LoadClusterPeerConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := before.TlsOptions(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"backend.json", "cluster/raft", "cluster/node.json"} {
		if _, err := os.Lstat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Fatalf("initialization started a service: %s: %v", name, err)
		}
	}
	if err := run([]string{"peer-init", "--state-dir", root}); err != nil {
		t.Fatal(err)
	}
	after, err := cluster.LoadClusterPeerConfig(path)
	if err != nil || before.ClusterID != after.ClusterID || before.NodeID != after.NodeID {
		t.Fatalf("retry changed identity: %v", err)
	}
}
