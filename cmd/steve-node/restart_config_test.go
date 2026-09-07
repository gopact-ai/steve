package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/adapter"
	"github.com/gopact-ai/steve/internal/node"
)

func TestRestartConfigPreservesEndpointInstanceAndOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.json")
	base := node.ServerConfig{Name: "n", Listen: "127.0.0.1:7701", Token: "private-auth", Hubs: map[string]string{"owner-token": "hub"}, StateDir: filepath.Join(t.TempDir(), "state"), Harnesses: map[string]node.HarnessSpec{"cat": {Command: "/bin/cat"}}}
	write := func(cfg node.ServerConfig) {
		t.Helper()
		raw, _ := json.Marshal(cfg)
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(base)
	running, err := load(path)
	if err != nil {
		t.Fatal(err)
	}
	running.Source = path
	for _, test := range []struct {
		name   string
		change func(*node.ServerConfig)
	}{
		{"name", func(c *node.ServerConfig) { c.Name = "other" }},
		{"listen", func(c *node.ServerConfig) { c.Listen = "127.0.0.1:7702" }},
		{"state", func(c *node.ServerConfig) { c.StateDir += "-other" }},
		{"token", func(c *node.ServerConfig) { c.Token = "other-private-auth" }},
		{"hubs", func(c *node.ServerConfig) { c.Hubs = map[string]string{"new-secret": "other"} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := base
			test.change(&changed)
			write(changed)
			err := checkRestartConfig(running, "")
			if err == nil {
				t.Fatal("restart accepted changed service identity")
			}
			if strings.Contains(err.Error(), "private-auth") || strings.Contains(err.Error(), "new-secret") {
				t.Fatal("preflight exposed credentials")
			}
		})
	}
	changed := base
	changed.Listen = "127.0.0.1:7702"
	write(changed)
	if err := checkRestartConfig(running, base.Listen); err != nil {
		t.Fatal("effective --listen override was not applied", err)
	}
	changed = base
	changed.Tools = []string{"git"}
	write(changed)
	if err := checkRestartConfig(running, ""); err != nil {
		t.Fatal("ordinary valid configuration change rejected", err)
	}
}

func TestRestartConfigRejectsChangedValidationSnapshotAndDoesNotInstall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.json")
	cfg := node.ServerConfig{Name: "n", Listen: "127.0.0.1:7701", Token: "auth", StateDir: filepath.Join(t.TempDir(), "never-created"), Harnesses: map[string]node.HarnessSpec{"cat": {Command: "/bin/cat"}}}
	raw, _ := json.Marshal(cfg)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := readNodeConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.unchanged(); err == nil {
		t.Fatal("changed source bytes reused a previous validation")
	}
	if err := os.WriteFile(path, append(raw, []byte(" {}")...), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := load(path); err == nil {
		t.Fatal("trailing configuration document accepted")
	}
	for name := range adapter.Catalog {
		cfg.Harnesses = map[string]node.HarnessSpec{"missing-adapter": {Adapter: name}}
		break
	}
	raw, _ = json.Marshal(cfg)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	running, err := load(path)
	if err != nil {
		t.Fatal(err)
	}
	running.Source = path
	if err := checkRestartConfig(running, ""); err == nil || !strings.Contains(err.Error(), "adapter installation") {
		t.Fatal("missing adapter silently delegated to startup download", err)
	}
	if _, err := os.Stat(cfg.StateDir); !os.IsNotExist(err) {
		t.Fatal("preflight initialized state or adapter directories", err)
	}
}

func TestRestartConfigRejectsEphemeralPortAndConflictingBroker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.json")
	cfg := node.ServerConfig{Name: "n", Listen: "127.0.0.1:0", Token: "auth", StateDir: filepath.Join(t.TempDir(), "state"), Harnesses: map[string]node.HarnessSpec{"cat": {Command: "/bin/cat"}}}
	raw, _ := json.Marshal(cfg)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cfg.Source = path
	if err := checkRestartConfig(cfg, ""); err == nil {
		t.Fatal("ephemeral endpoint can change during controlled restart")
	}
	cfg.Listen = "127.0.0.1:7701"
	cfg.MCPBroker = &node.BrokerRef{Socket: "/unused/external.sock"}
	cfg.MCPServers = map[string]node.MCPSpec{"local": {Command: "/bin/cat"}}
	raw, _ = json.Marshal(cfg)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := checkRestartConfig(cfg, ""); err == nil || !strings.Contains(err.Error(), "exclusive") {
		t.Fatal("startup broker conflict accepted", err)
	}
}
