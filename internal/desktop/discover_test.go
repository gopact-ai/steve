package desktop

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gopact-ai/steve/internal/config"
)

func TestDiscoveryDoesNotRunAgentsOrReadTheirPrivateData(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "should-not-exist")
	for _, name := range []string{"codex", "claude", "grok", "kimi", "node", "npm"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\ntouch '"+marker+"'\nexit 99\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	candidates := DiscoverAgents(DiscoveryOptions{Path: bin, HomeDir: root})
	if len(candidates) != 4 {
		t.Fatalf("candidates=%d", len(candidates))
	}
	for _, item := range candidates {
		if !item.Installed || item.Executable == "" {
			t.Fatalf("missed installed tool: %+v", item)
		}
		a, h, err := Registration(item)
		if err != nil {
			t.Fatal(err)
		}
		if a.Harness != item.Harness || h.Permission != config.PermissionRead || a.Default {
			t.Fatalf("registration did not preserve explicit choice: %+v, %+v", a, h)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("discovery or registration executed a tool")
	}
	if _, err := os.Stat(filepath.Join(root, ".codex")); !os.IsNotExist(err) {
		t.Fatal("discovery prepared agent state")
	}
}

func TestDiscoveryReportsMissingAdapterRuntimeAndRejectsStaleExecutable(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "codex"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	items := DiscoverAgents(DiscoveryOptions{Path: root, HomeDir: t.TempDir()})
	var found AgentCandidate
	for _, item := range items {
		if item.ID == "codex" {
			found = item
		}
	}
	if !found.Installed || len(found.Requires) == 0 {
		t.Fatalf("missing dependencies were hidden: %+v", found)
	}
	if _, _, err := Registration(found); err == nil {
		t.Fatal("registered tool without the adapter runtime")
	}
	if err := os.Remove(found.Executable); err != nil {
		t.Fatal(err)
	}
	found.Requires = nil
	if _, _, err := Registration(found); err == nil {
		t.Fatal("registered an executable removed after discovery")
	}
}
