package fleetlab

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHubCloseReportsRemovalErrorsAndAttemptsEveryResource(t *testing.T) {
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + calls + "'\ncase \"$*\" in 'rm --force --volumes broken') echo injected-removal-failure >&2; exit 42;; esac\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	hubDir, err := os.MkdirTemp(t.TempDir(), "hub-")
	if err != nil {
		t.Fatal(err)
	}
	nodeDir, err := os.MkdirTemp(t.TempDir(), "node-")
	if err != nil {
		t.Fatal(err)
	}
	h := &Hub{Dir: hubDir, lab: &docker{work: nodeDir, network: "owned-network", containers: map[string]string{"a": "broken", "b": "remaining"}}}
	if err := h.Close(); err == nil || !strings.Contains(err.Error(), "injected-removal-failure") {
		t.Fatalf("cleanup hid error: %v", err)
	}
	raw, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range []string{"rm --force --volumes broken", "rm --force --volumes remaining", "network rm owned-network"} {
		if !strings.Contains(string(raw), call) {
			t.Errorf("cleanup did not attempt %q: %s", call, raw)
		}
	}
	for _, path := range []string{hubDir, nodeDir} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("cleanup did not remove %s: %v", path, err)
		}
	}
}
