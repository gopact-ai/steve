package cluster

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gopact-ai/steve/internal/config"
	runtimestore "github.com/gopact-ai/steve/internal/runtime"
)

func TestInitializePeerPreservesIdentityConfigurationAndSecrets(t *testing.T) {
	root := filepath.Join(t.TempDir(), "service")
	first, err := InitializePeer(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(first.Config)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Gateway.Locale = "en"
	if err := config.Save(first.Config, cfg); err != nil {
		t.Fatal(err)
	}
	before := peerIdentityFiles(t, first)
	second, err := InitializePeer(root)
	if err != nil || first != second {
		t.Fatalf("retry: %+v, %v", second, err)
	}
	after := peerIdentityFiles(t, second)
	for name, data := range before {
		if !bytes.Equal(data, after[name]) {
			t.Fatalf("retry replaced %s", name)
		}
	}
	raw, _ := json.Marshal(second)
	for _, name := range []string{first.TokenFile, filepath.Join(root, "cluster", "owner-control-token"), filepath.Join(root, "cluster", "ca-key.pem")} {
		if bytes.Contains(raw, before[name]) {
			t.Fatal("initialization result contains a credential")
		}
	}
}

func TestInitializePeerRejectsUnknownOrUnsafeDirectoriesWithoutChangingThem(t *testing.T) {
	for _, kind := range []string{"unknown", "standalone", "public", "symlink", "lock-symlink"} {
		t.Run(kind, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			if err := os.Mkdir(root, 0700); err != nil {
				t.Fatal(err)
			}
			name := "keep.txt"
			if kind == "standalone" {
				name = "config.json"
			}
			if err := os.WriteFile(filepath.Join(root, name), []byte("keep unchanged"), 0600); err != nil {
				t.Fatal(err)
			}
			path := root
			switch kind {
			case "public":
				if err := os.Chmod(root, 0755); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				path = root + "-link"
				if err := os.Symlink(root, path); err != nil {
					t.Fatal(err)
				}
			case "lock-symlink":
				if err := os.Symlink(filepath.Join(root, name), filepath.Join(root, "gateway.lock")); err != nil {
					t.Fatal(err)
				}
			}
			entries, _ := os.ReadDir(root)
			if _, err := InitializePeer(path); err == nil {
				t.Fatal("accepted unsafe or unknown state directory")
			}
			after, _ := os.ReadDir(root)
			data, err := os.ReadFile(filepath.Join(root, name))
			if err != nil || string(data) != "keep unchanged" || len(entries) != len(after) {
				t.Fatal("initialization changed an existing directory")
			}
		})
	}
}

func TestInitializePeerRejectsCorruptExistingAuthority(t *testing.T) {
	for _, kind := range []string{"sidecar", "sidecar-symlink", "identity", "certificate", "ca-key", "owner-token", "listener"} {
		t.Run(kind, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			result, err := InitializePeer(root)
			if err != nil {
				t.Fatal(err)
			}
			peer, err := LoadClusterPeerConfig(result.ClusterConfig)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "sidecar":
				err = os.WriteFile(result.ClusterConfig, []byte("broken"), 0600)
			case "sidecar-symlink":
				err = os.Rename(result.ClusterConfig, result.ClusterConfig+".saved")
				if err == nil {
					err = os.Symlink(result.ClusterConfig+".saved", result.ClusterConfig)
				}
			case "identity":
				peer.NodeID = "other-node"
				err = SaveClusterJSON(result.ClusterConfig, peer, false)
			case "certificate":
				err = os.WriteFile(peer.CertFile, []byte("broken"), 0600)
			case "ca-key":
				err = os.WriteFile(peer.CAKeyFile, []byte("broken"), 0600)
			case "owner-token":
				err = os.WriteFile(peer.OwnerTokenFile, []byte("too-short"), 0600)
			case "listener":
				peer.UIAddress = "127.0.0.1:7777"
				err = SaveClusterJSON(result.ClusterConfig, peer, false)
			}
			if err != nil {
				t.Fatal(err)
			}
			before := peerIdentityFiles(t, result)
			if _, err := InitializePeer(root); err == nil {
				t.Fatal("accepted corrupt cluster installation")
			}
			for name, data := range before {
				after, err := os.ReadFile(name)
				if err != nil || !bytes.Equal(data, after) {
					t.Fatalf("replaced %s", name)
				}
			}
		})
	}
}

func TestInitializePeerRefusesInactivePeerProcess(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	if _, err := InitializePeer(root); err != nil {
		t.Fatal(err)
	}
	unlock, err := runtimestore.AcquireLock(filepath.Join(root, "cluster", "peer-process"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	lockPath := filepath.Join(root, "gateway.lock")
	if err := os.WriteFile(lockPath, []byte("previous application"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := InitializePeer(root); err == nil {
		t.Fatal("initialized while peer process lock was held")
	}
	if data, err := os.ReadFile(lockPath); err != nil || string(data) != "previous application" {
		t.Fatal("refusing a live peer touched its application lock")
	}
}

func TestInitializePeerValidatesRecoveryBeforePublishingSidecar(t *testing.T) {
	for _, kind := range []string{"symlink", "bad-owner", "foreign-path", "missing-token"} {
		t.Run(kind, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			result, err := InitializePeer(root)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(result.ClusterConfig); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(root, "cluster")
			switch kind {
			case "symlink":
				if err := os.Rename(dir, dir+"-saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(dir+"-saved", dir); err != nil {
					t.Fatal(err)
				}
			case "bad-owner":
				if err := os.WriteFile(filepath.Join(dir, "owner-control-token"), []byte("bad"), 0600); err != nil {
					t.Fatal(err)
				}
			case "foreign-path":
				peer, err := LoadClusterPeerConfig(filepath.Join(dir, "bootstrap.json"))
				if err != nil {
					t.Fatal(err)
				}
				peer.WorkerConfigFile = filepath.Join(t.TempDir(), "node.json")
				if err := SaveClusterJSON(filepath.Join(dir, "bootstrap.json"), peer, false); err != nil {
					t.Fatal(err)
				}
			case "missing-token":
				if err := os.Remove(result.TokenFile); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := InitializePeer(root); err == nil {
				t.Fatal("accepted invalid recovery state")
			}
			if _, err := os.Lstat(result.ClusterConfig); !os.IsNotExist(err) {
				t.Fatal("published sidecar before validation")
			}
			if kind == "missing-token" {
				if _, err := os.Lstat(result.TokenFile); !os.IsNotExist(err) {
					t.Fatal("replaced missing credential")
				}
			}
		})
	}
}

func TestInitializePeerRecoversPublishedAuthorityButNotMissingLiveSidecar(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	first, err := InitializePeer(root)
	if err != nil {
		t.Fatal(err)
	}
	before := peerIdentityFiles(t, first)
	if err := os.Remove(first.ClusterConfig); err != nil {
		t.Fatal(err)
	}
	second, err := InitializePeer(root)
	if err != nil || first != second {
		t.Fatalf("recovery: %+v %v", second, err)
	}
	for name, data := range before {
		after, err := os.ReadFile(name)
		if err != nil || !bytes.Equal(data, after) {
			t.Fatalf("recovery replaced %s", name)
		}
	}
	if err := os.Mkdir(filepath.Join(root, "cluster", "raft"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(first.ClusterConfig); err != nil {
		t.Fatal(err)
	}
	if _, err := InitializePeer(root); err == nil {
		t.Fatal("recreated sidecar for initialized Raft")
	}
	if _, err := os.Lstat(first.ClusterConfig); !os.IsNotExist(err) {
		t.Fatal("published sidecar despite existing Raft")
	}
}

func TestInitializePeerRefusesConcurrentApplication(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	first, err := InitializePeer(root)
	if err != nil {
		t.Fatal(err)
	}
	before := peerIdentityFiles(t, first)
	unlock, err := runtimestore.AcquireLock(root)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := InitializePeer(root); err == nil {
		t.Fatal("initialized while application lock was held")
	}
	for name, data := range before {
		after, err := os.ReadFile(name)
		if err != nil || !bytes.Equal(data, after) {
			t.Fatalf("replaced %s", name)
		}
	}
}

func peerIdentityFiles(t *testing.T, result PeerInitialization) map[string][]byte {
	t.Helper()
	files := []string{result.Config, result.ClusterConfig, result.TokenFile}
	for _, name := range []string{"ca.pem", "ca-key.pem", "node.pem", "node-key.pem", "owner-control-token"} {
		files = append(files, filepath.Join(filepath.Dir(result.Config), "cluster", name))
	}
	resultFiles := map[string][]byte{}
	for _, name := range files {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		resultFiles[name] = data
	}
	return resultFiles
}
