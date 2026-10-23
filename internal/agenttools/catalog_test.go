package agenttools

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
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
	paths := map[string]string{"codex": "/remote/private/bin/codex", "node": "node", "npm": "/remote/private/bin/npm", "pi": "./pi"}
	out := FromPaths(paths)
	if len(out) != 3 {
		t.Fatalf("catalog=%+v", out)
	}
	for _, candidate := range out {
		if candidate.ID == "codex" && (!candidate.Installed || len(candidate.Requires) != 1 || candidate.Requires[0] != "node") {
			t.Fatalf("remote mapped candidate=%+v", candidate)
		}
		if candidate.ID == "pi" && candidate.Installed {
			t.Fatal("relative remote executable accepted")
		}
	}
}

// Steve supports exactly three agent tools, each through a pinned adapter.
func TestCatalogListsExactlyCodexClaudeAndPi(t *testing.T) {
	var ids, adapters []string
	for _, candidate := range Catalog() {
		ids = append(ids, candidate.ID)
		adapters = append(adapters, candidate.Adapter)
	}
	if !slices.Equal(ids, []string{"codex", "claude", "pi"}) || !slices.Equal(adapters, []string{"codex-acp", "claude-agent-acp", "pi-acp"}) {
		t.Fatalf("catalog = %v adapters %v", ids, adapters)
	}
	pi, ok := Lookup("pi")
	if !ok || pi.Name != "pi" || pi.Harness != "pi" {
		t.Fatalf("pi = %+v", pi)
	}
	for _, retired := range []string{"kimi", "grok", "dsh"} {
		if _, ok := Lookup(retired); ok {
			t.Fatalf("%s is still offered", retired)
		}
		if _, err := Registration(Candidate{ID: retired, Harness: retired, Installed: true, Executable: "/bin/sh"}); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("%s registration: %v", retired, err)
		}
	}
}

// Discovery never offers a retired tool, even where its binary is present.
func TestDiscoveryIgnoresRetiredTools(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"kimi", "grok", "dsh"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, candidate := range Discover(Options{Path: dir}) {
		if candidate.Installed {
			t.Fatalf("offered %+v", candidate)
		}
	}
}

// pi is driven by pi-acp, which is told which pi this machine found.
func TestPiRegistrationPassesTheDetectedExecutable(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"pi", "node", "npm"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	var pi Candidate
	for _, candidate := range Discover(Options{Path: dir}) {
		if candidate.ID == "pi" {
			pi = candidate
		}
	}
	declared, err := Registration(pi)
	if err != nil {
		t.Fatal(err)
	}
	if declared.Adapter != "pi-acp" || declared.Command != "" || len(declared.Args) != 0 || !slices.Equal(declared.Env, []string{"PI_ACP_PI_COMMAND=" + filepath.Join(dir, "pi")}) {
		t.Fatalf("declared = %+v", declared)
	}
	pi.Requires = []string{"node"}
	if _, err := Registration(pi); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("pi without node: %v", err)
	}
}

func TestToolEnvMovedOnlyForTheDeclaredPath(t *testing.T) {
	declared := []string{PiCommandEnv + "=/new/pi"}
	for _, tc := range []struct {
		stored         []string
		moved, current bool
	}{
		{[]string{"A=1", PiCommandEnv + "=/old/pi"}, true, false},
		{[]string{"A=1", PiCommandEnv + "=/new/pi"}, false, true},
		{[]string{"A=1"}, false, false},
		{nil, false, false},
	} {
		if got := ToolEnvMoved(tc.stored, declared); got != tc.moved {
			t.Fatalf("ToolEnvMoved(%v) = %v", tc.stored, got)
		}
		if got := ToolEnvCurrent(tc.stored, declared); got != tc.current {
			t.Fatalf("ToolEnvCurrent(%v) = %v", tc.stored, got)
		}
	}
	if got := WithToolEnv([]string{"A=1", PiCommandEnv + "=/old/pi", "B=2"}, declared); !slices.Equal(got, []string{"A=1", "B=2", PiCommandEnv + "=/new/pi"}) {
		t.Fatalf("WithToolEnv = %v", got)
	}
}
