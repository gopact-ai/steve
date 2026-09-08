package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gopact-ai/steve/internal/cluster"
)

func TestClusterPeerDoesNotOptOrdinaryCLIIntoClusterMode(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	handled, err := maybeManagedPeer([]string{"--config", configPath})
	if handled || err != nil {
		t.Fatalf("ordinary CLI unexpectedly entered managed mode: %v %v", handled, err)
	}
	if _, err := os.Stat(cluster.DefaultClusterConfigPath(configPath)); !os.IsNotExist(err) {
		t.Fatalf("ordinary CLI created cluster configuration: %v", err)
	}
}
