package plugins

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func cleanupSocketDir(t *testing.T, socket string) {
	t.Helper()
	if !strings.HasPrefix(filepath.Base(filepath.Dir(socket)), "steve-plugin-socket-") || filepath.Base(socket) != "mcp.sock" {
		t.Fatalf("not a test-owned socket: %s", socket)
	}
	t.Cleanup(func() {
		if err := os.Remove(socket); err != nil && !os.IsNotExist(err) {
			t.Error(err)
		}
		if err := os.Remove(filepath.Dir(socket)); err != nil && !os.IsNotExist(err) {
			t.Error(err)
		}
	})
}

func TestRuntimeSocketBindsAndStaysStableWithLongTMPDIR(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix runtime sockets")
	}
	store, record := runtimeUsageFixture(t)
	longTemp := filepath.Join(t.TempDir(), strings.Repeat("t", 120))
	if err := os.Mkdir(longTemp, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", longTemp)
	socket, err := store.RuntimeSocket(t.Context(), record.Ref)
	if err != nil {
		t.Fatal(err)
	}
	cleanupSocketDir(t, socket)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatalf("runtime socket must bind despite long TMPDIR (%d bytes): %v", len(socket), err)
	}
	t.Cleanup(func() { listener.Close() })
	info, err := os.Lstat(filepath.Dir(socket))
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		t.Fatalf("socket directory must stay private: %v %v", info, err)
	}
	reopened := &Store{Dir: store.Dir}
	replayed, err := reopened.RuntimeSocket(t.Context(), record.Ref)
	if err != nil || replayed != socket {
		t.Fatalf("runtime socket identity changed: %q %q %v", socket, replayed, err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	rebound, err := net.ListenUnix("unix", &net.UnixAddr{Name: replayed, Net: "unix"})
	if err != nil {
		t.Fatalf("stable address cannot be rebound: %v", err)
	}
	t.Cleanup(func() { rebound.Close() })
}
