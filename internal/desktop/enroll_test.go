package desktop

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gopact-ai/steve/internal/config"
)

func enrollState(t *testing.T) *Installation {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "steve")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	installed, err := Bootstrap(Options{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	return installed
}

func fakeTools(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestEnrollDiscoveredRegistersRunnableAgentsOnce(t *testing.T) {
	installed := enrollState(t)
	candidates := DiscoverAgents(DiscoveryOptions{Path: fakeTools(t, "codex", "claude", "node", "npm")})
	added, err := EnrollDiscovered(installed.Paths.Config, candidates)
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 2 {
		t.Fatalf("added %v, want Codex and Claude Code", added)
	}
	cfg, err := config.Load(installed.Paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	defaults := 0
	for id, a := range cfg.Agents {
		if a.Default {
			defaults++
		}
		if got := cfg.Harnesses[a.Harness].Permission; got != config.PermissionRead {
			t.Fatalf("%s starts with %q, want read", id, got)
		}
	}
	if defaults != 1 {
		t.Fatalf("%d default agents, want one", defaults)
	}
	again, err := EnrollDiscovered(installed.Paths.Config, candidates)
	if err != nil || len(again) != 0 {
		t.Fatalf("second run added %v (%v); an owner's agents must be left alone", again, err)
	}
}

func TestEnrollDiscoveredSkipsToolsMissingRequirements(t *testing.T) {
	installed := enrollState(t)
	// node and npm are absent, so the Codex adapter cannot run.
	added, err := EnrollDiscovered(installed.Paths.Config, DiscoverAgents(DiscoveryOptions{Path: fakeTools(t, "codex")}))
	if err != nil || len(added) != 0 {
		t.Fatalf("added %v (%v), want nothing", added, err)
	}
	cfg, err := config.Load(installed.Paths.Config)
	if err != nil || len(cfg.Agents) != 0 {
		t.Fatalf("configuration changed: %v (%v)", cfg.Agents, err)
	}
}
