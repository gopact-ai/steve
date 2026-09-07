package desktop

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gopact-ai/steve/internal/config"
)

func TestBootstrapCreatesPrivateUsableLocalInstallation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Steve")
	installed, err := Bootstrap(Options{StateDir: root})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(installed.Paths.Config)
	if err != nil {
		t.Fatalf("first launch configuration cannot start: %v", err)
	}
	if err := cfg.ValidateChannels(); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Agents) != 0 || len(cfg.Harnesses) != 0 || cfg.FeishuEnabled() {
		t.Fatal("first launch registered or enabled an unselected service")
	}
	if cfg.Gateway.HubID != installed.NodeID || !strings.HasPrefix(installed.NodeID, "node-") {
		t.Fatalf("local coordinator has no durable node identity: %q", cfg.Gateway.HubID)
	}
	if len(installed.Token) < 40 || cfg.Gateway.ReadModelToken != installed.Token || !strings.HasPrefix(installed.URL, "http://127.0.0.1:") {
		t.Fatal("first launch did not establish authenticated loopback access")
	}
	for path, mode := range map[string]os.FileMode{root: 0o700, installed.Paths.Config: 0o600, installed.Paths.Profile: 0o600, installed.Paths.Identity: 0o600, installed.Paths.Token: 0o600} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("private path %s: info=%v, error=%v", filepath.Base(path), info, err)
		}
	}
	if !IsManagedConfig(installed.Paths.Config) {
		t.Fatal("desktop startup cannot identify its managed configuration")
	}
	if _, err := os.Stat(filepath.Join(root, "runtimes")); !os.IsNotExist(err) {
		t.Fatal("bootstrap prepared or copied an unselected agent runtime")
	}
	encoded, _ := json.Marshal(installed)
	if strings.Contains(string(encoded), installed.Token) {
		t.Fatal("normal installation serialization disclosed its token")
	}
}

func TestBootstrapKeepsIdentityTokenAndUserConfiguration(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Steve")
	first, err := Bootstrap(Options{StateDir: root})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(first.Paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Gateway.Locale = "en"
	cfg.Gateway.TaskMaxTurns = 91
	if err := config.Save(first.Paths.Config, cfg); err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile(first.Paths.Config)
	second, err := Bootstrap(Options{StateDir: root})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(first.Paths.Config)
	if second.NodeID != first.NodeID || second.Token != first.Token || second.URL != first.URL || string(got) != string(want) {
		t.Fatal("relaunch rewrote user configuration or installation identity")
	}
}

func TestBootstrapRejectsUnownedConfigurationAndSymlinks(t *testing.T) {
	for _, kind := range []string{"unowned-config", "linked-config", "linked-root"} {
		t.Run(kind, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "Steve")
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(base, "important.json")
			if err := os.WriteFile(target, []byte("untouched"), 0o600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "unowned-config":
				if err := os.WriteFile(filepath.Join(root, "config.json"), []byte("untouched"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "linked-config":
				if err := os.Symlink(target, filepath.Join(root, "config.json")); err != nil {
					t.Fatal(err)
				}
			case "linked-root":
				linked := filepath.Join(base, "linked")
				if err := os.Symlink(root, linked); err != nil {
					t.Fatal(err)
				}
				root = linked
			}
			if _, err := Bootstrap(Options{StateDir: root}); err == nil {
				t.Fatal("unsafe installation path accepted")
			}
			got, _ := os.ReadFile(target)
			if string(got) != "untouched" {
				t.Fatal("bootstrap modified another file")
			}
		})
	}
}

func TestConcurrentBootstrapKeepsOneInstallation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Steve")
	var wg sync.WaitGroup
	results := make(chan *Installation, 4)
	errors := make(chan error, 4)
	for range 4 {
		wg.Go(func() {
			value, err := Bootstrap(Options{StateDir: root})
			if err != nil {
				errors <- err
			} else {
				results <- value
			}
		})
	}
	wg.Wait()
	close(results)
	close(errors)
	for err := range errors {
		t.Fatal(err)
	}
	var first *Installation
	for got := range results {
		if first == nil {
			first = got
		}
		if first.NodeID != got.NodeID || first.Token != got.Token || first.URL != got.URL {
			t.Fatal("first launch split into multiple identities")
		}
	}
}

func TestFirstBoundPortIsRememberedForStableWebViewStorage(t *testing.T) {
	installed, err := Bootstrap(Options{StateDir: filepath.Join(t.TempDir(), "Steve")})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(installed.Paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	if err := PinAddress(installed.Paths.Config, cfg, "http://127.0.0.1:41731"); err != nil {
		t.Fatal(err)
	}
	reopened, err := Bootstrap(Options{StateDir: installed.Paths.Root})
	if err != nil {
		t.Fatal(err)
	}
	if reopened.URL != "http://127.0.0.1:41731" {
		t.Fatal("relaunch would use a different browser origin")
	}
	if err := PinAddress(installed.Paths.Config, cfg, "http://127.0.0.1:41732"); err == nil {
		t.Fatal("a later bind silently moved browser storage to another origin")
	}
}
