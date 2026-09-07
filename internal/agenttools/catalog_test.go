package agenttools

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSharedDiscoveryFindsOnlyExecutableFilesWithoutRunningThem(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "executed")
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte("not executable"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range Discover(Options{Path: dir}) {
		if candidate.ID == "codex" && (!candidate.Installed || len(candidate.Requires) != 2) {
			t.Fatalf("codex=%+v", candidate)
		}
		if candidate.ID == "claude" && candidate.Installed {
			t.Fatal("nonexecutable file discovered")
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("discovery ran a program")
	}
}

func TestRemotePathMappingSharesTheCatalogWithoutAccessingLocalFiles(t *testing.T) {
	paths := map[string]string{"codex": "/remote/private/bin/codex", "node": "node", "npm": "/remote/private/bin/npm", "grok": "./grok"}
	out := FromPaths(paths)
	if len(out) != 4 {
		t.Fatalf("catalog=%+v", out)
	}
	for _, candidate := range out {
		if candidate.ID == "codex" && (!candidate.Installed || len(candidate.Requires) != 1 || candidate.Requires[0] != "node") {
			t.Fatalf("remote mapped candidate=%+v", candidate)
		}
		if candidate.ID == "grok" && candidate.Installed {
			t.Fatal("relative remote executable accepted")
		}
	}
}
