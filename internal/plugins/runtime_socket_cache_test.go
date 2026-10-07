package plugins

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRuntimeSocketPreservesValidCachedAddressWithLongTMPDIR(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix runtime sockets")
	}
	store, record := runtimeUsageFixture(t)
	dir, err := os.MkdirTemp("/tmp", "steve-plugin-socket-cached-")
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "mcp.sock")
	cleanupSocketDir(t, socket)
	raw, err := json.Marshal(socket)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.RuntimeDir(record.Ref.ID), "socket.json")
	if err := store.writeRecord(path, raw); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), strings.Repeat("t", 120)))
	replayed, err := store.RuntimeSocket(t.Context(), record.Ref)
	if err != nil || replayed != socket {
		t.Fatalf("valid cached address changed: %q %q %v", socket, replayed, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("valid cached record rewritten: %v", err)
	}
}

func TestRuntimeSocketRejectsOversizedCacheWithoutRewriting(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix runtime sockets")
	}
	store, record := runtimeUsageFixture(t)
	socket := filepath.Join(t.TempDir(), "steve-plugin-socket-"+strings.Repeat("x", 120), "mcp.sock")
	raw, err := json.Marshal(socket)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.RuntimeDir(record.Ref.ID), "socket.json")
	if err := store.writeRecord(path, raw); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RuntimeSocket(t.Context(), record.Ref); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("oversized cached address accepted: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("invalid cached record rewritten: %v", err)
	}
	if _, err := os.Lstat(filepath.Dir(socket)); !os.IsNotExist(err) {
		t.Fatalf("invalid socket directory materialized: %v", err)
	}
}

func TestRuntimeSocketPathLimitCountsBytesAndTerminator(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix runtime sockets")
	}
	limit := 104
	if runtime.GOOS == "linux" {
		limit = 108
	}
	prefix, suffix := "/tmp/steve-plugin-socket-", "/mcp.sock"
	longest := prefix + strings.Repeat("x", limit-1-len(prefix)-len(suffix)) + suffix
	if !validRuntimeSocketPath(longest) || validRuntimeSocketPath(strings.Replace(longest, "x", "xx", 1)) {
		t.Fatal("Unix pathname limit failed to reserve its terminator")
	}
	unicode := prefix + strings.Repeat("界", 30) + suffix
	if validRuntimeSocketPath(unicode) {
		t.Fatal("Unix pathname limit counted runes instead of bytes")
	}
}
