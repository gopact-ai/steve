package desktop

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBundledNodeBinaryMovesWithTheApplication(t *testing.T) {
	base := t.TempDir()
	resources := filepath.Join(base, "Download.app", "Contents", "Resources")
	want := filepath.Join(resources, "node-binaries", "linux-arm64", "steve-node")
	if err := os.MkdirAll(filepath.Dir(want), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(want, []byte("test binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	got, ok := bundledNodeBinary(filepath.Join(resources, "steve"), "linux/arm64")
	if !ok || got != want {
		t.Fatalf("bundle was not found: %q %v", got, ok)
	}
	destination := filepath.Join(base, "Installed.app")
	if err := os.Rename(filepath.Join(base, "Download.app"), destination); err != nil {
		t.Fatal(err)
	}
	got, ok = bundledNodeBinary(filepath.Join(destination, "Contents", "Resources", "steve"), "linux/arm64")
	if !ok || got != filepath.Join(destination, "Contents", "Resources", "node-binaries", "linux-arm64", "steve-node") {
		t.Fatal("moving the app left the node package pinned to its old location")
	}
}

func TestBundledNodeBinaryRejectsUnsupportedPlatformsAndPaths(t *testing.T) {
	for _, platform := range []string{"", "../amd64", "linux/../../bin", "linux/mips", "windows/amd64"} {
		if path, ok := bundledNodeBinary("/Applications/Steve.app/Contents/Resources/steve", platform); ok || path != "" {
			t.Fatalf("unsupported node package was exposed: %q", platform)
		}
	}
	if _, ok := bundledNodeBinary(filepath.Join(t.TempDir(), "steve"), "linux/amd64"); ok {
		t.Fatal("missing package was advertised")
	}
}

func TestReleaseLayoutServesOnlyTheSamePlatform(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"steve", "steve-node"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("test binary"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	backend := filepath.Join(dir, "steve")
	if got, ok := bundledPeerBinary(backend, hostPlatform); !ok || got != backend {
		t.Fatalf("release peer binary not found: %q %v", got, ok)
	}
	if got, ok := bundledNodeBinary(backend, hostPlatform); !ok || got != filepath.Join(dir, "steve-node") {
		t.Fatalf("release node binary not found: %q %v", got, ok)
	}
	other := "linux/amd64"
	if hostPlatform == other {
		other = "darwin/arm64"
	}
	if _, ok := bundledPeerBinary(backend, other); ok {
		t.Fatal("a release archive offered its own binary for another platform")
	}
}
