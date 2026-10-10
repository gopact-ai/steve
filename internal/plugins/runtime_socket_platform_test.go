package plugins

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeSocketWindowsKeepsOriginalPolicy(t *testing.T) {
	if got := runtimeSocketTempBase("windows"); got != "" {
		t.Errorf("Windows temp allocation changed: %q", got)
	}
	location := filepath.Join(t.TempDir(), "steve-plugin-socket-"+strings.Repeat("x", 130), "mcp.sock")
	if !validRuntimeSocketPath(location, "windows") {
		t.Error("Windows gained a Unix pathname limit on its cached address")
	}
	if validRuntimeSocketPath(filepath.Join(t.TempDir(), "other", "mcp.sock"), "windows") {
		t.Error("Windows lost its original cached-address shape validation")
	}
	for _, goos := range []string{"linux", "darwin"} {
		if runtimeSocketTempBase(goos) != "/tmp" || validRuntimeSocketPath(location, goos) {
			t.Fatalf("%s socket policy did not stay Unix-specific", goos)
		}
	}
}
