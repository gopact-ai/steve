package agenttools

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func pathTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("NVM_DIR", "")
	t.Setenv("NVM_BIN", "")
	return home
}

func pathTestProgram(t *testing.T, dir, name, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFinderDiscoveryIncludesUserInstallersWithoutExecuting(t *testing.T) {
	home := pathTestHome(t)
	marker := filepath.Join(home, "unexpected-execution")
	body := "#!/bin/sh\n: > '" + marker + "'\n"
	nvm := filepath.Join(home, ".nvm", "versions", "node", "v22.22.0", "bin")
	want := map[string]string{}
	for _, name := range []string{"codex", "claude", "node", "npm"} {
		want[name] = pathTestProgram(t, nvm, name, body)
	}
	want["kimi"] = pathTestProgram(t, filepath.Join(home, ".kimi-code", "bin"), "kimi", body)
	want["grok"] = pathTestProgram(t, filepath.Join(home, ".local", "bin"), "grok", body)
	// Keep this assertion independent of tools installed in the test host's
	// system directories, while using the actual sparse-environment expansion.
	var fixtureDirs []string
	for _, dir := range filepath.SplitList(ExecutablePath(home)) {
		if strings.HasPrefix(dir, home+string(filepath.Separator)) {
			fixtureDirs = append(fixtureDirs, dir)
		}
	}
	for _, candidate := range Discover(Options{Path: strings.Join(fixtureDirs, string(os.PathListSeparator))}) {
		if !candidate.Installed || candidate.Executable != want[candidate.ID] || len(candidate.Requires) != 0 {
			t.Errorf("candidate %+v, expected executable %s with available runtime", candidate, want[candidate.ID])
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("discovery executed an installed program")
	}
}

func TestNVMSearchOrdersNumericVersionsAndHonorsExplicitPath(t *testing.T) {
	home := pathTestHome(t)
	root := filepath.Join(home, ".nvm", "versions", "node")
	for _, version := range []string{"v9.99.0", "v22.22.0", "v100.1.0", "v100.2.0", "v100.2.1", "v999.0.0-invalid", "v999.0", "999.0.0", "v0999.0.0"} {
		pathTestProgram(t, filepath.Join(root, version, "bin"), "steve-test-tool", "#!/bin/sh\nexit 1\n")
	}
	search := ExecutablePath(home)
	if got := findExecutable(search, "steve-test-tool"); got != filepath.Join(root, "v100.2.1", "bin", "steve-test-tool") {
		t.Fatalf("numeric fallback selected %s", got)
	}
	for _, invalid := range []string{"v999.0.0-invalid", "v999.0", "999.0.0", "v0999.0.0"} {
		if slices.Contains(filepath.SplitList(search), filepath.Join(root, invalid, "bin")) {
			t.Errorf("accepted invalid version %s", invalid)
		}
	}
	older := filepath.Join(root, "v9.99.0", "bin")
	t.Setenv("PATH", older+":/usr/bin:/bin")
	if got := findExecutable(ExecutablePath(home), "steve-test-tool"); got != filepath.Join(older, "steve-test-tool") {
		t.Fatalf("explicit PATH lost priority: %s", got)
	}
}

func TestExecutablePathUsesAbsoluteNVMOverridesAndIsStable(t *testing.T) {
	home := pathTestHome(t)
	nvmRoot := t.TempDir()
	nvmBin := t.TempDir()
	versionBin := filepath.Join(nvmRoot, "versions", "node", "v20.1.0", "bin")
	pathTestProgram(t, versionBin, "steve-test-tool", "#!/bin/sh\nexit 1\n")
	t.Setenv("NVM_DIR", nvmRoot)
	t.Setenv("NVM_BIN", nvmBin)
	t.Setenv("PATH", "/usr/bin:.:relative:/usr/bin/../bin:")
	first := ExecutablePath(home)
	parts := filepath.SplitList(first)
	if !slices.Contains(parts, nvmBin) || !slices.Contains(parts, versionBin) {
		t.Fatalf("absolute NVM overrides absent: %v", parts)
	}
	seen := map[string]bool{}
	for _, part := range parts {
		if !filepath.IsAbs(part) || seen[part] {
			t.Fatalf("relative or repeated search directory: %q", part)
		}
		seen[part] = true
	}
	t.Setenv("PATH", first)
	if second := ExecutablePath(home); first != second {
		t.Fatalf("augmentation changes an already supplemented PATH: %s", second)
	}
	t.Setenv("NVM_DIR", "relative-nvm")
	t.Setenv("NVM_BIN", "relative-bin")
	if got := ExecutablePath("relative-home"); strings.Contains(got, "relative-") {
		t.Fatalf("accepted relative installer roots: %s", got)
	}
}

func TestInitializedPathSupportsInstalledEnvShebangAndChildProcesses(t *testing.T) {
	home := pathTestHome(t)
	nvm := filepath.Join(home, ".nvm", "versions", "node", "v22.22.0", "bin")
	// Unique fixture names avoid invoking any installed npm, Node or agent.
	pathTestProgram(t, nvm, "steve-test-node", "#!/bin/sh\nprintf 'fixture-runtime-ready'\n")
	pathTestProgram(t, nvm, "steve-test-npm", "#!/usr/bin/env steve-test-node\n")
	if err := InitializePath(); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("steve-test-npm").CombinedOutput()
	if err != nil || string(out) != "fixture-runtime-ready" {
		t.Fatalf("installed child runtime failed: %v, %s", err, out)
	}
}
