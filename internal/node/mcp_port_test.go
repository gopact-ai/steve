//go:build unix

package node

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/stableport"
)

func requireStablePort(t *testing.T, what string, port int) {
	t.Helper()
	if !stableport.InRange(port) {
		t.Fatalf("%s bound port %d, outside the stable range", what, port)
	}
}

// The worker's MCP port is remembered and bound again on restart, so the
// first one comes from the stable range, where the kernel does not hand
// it to other sockets while the worker is down.
func TestReverseMCPPicksItsFirstPortFromTheStableRange(t *testing.T) {
	s := NewServer(ServerConfig{Name: "n", Token: "token", StateDir: t.TempDir()})
	t.Cleanup(s.closeMCP)
	listener, err := s.listenMCP()
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	requireStablePort(t, "reverse MCP", port)
	if remembered := rememberedPort(s.conf()); remembered != port {
		t.Fatalf("remembered %d, bound %d", remembered, port)
	}
}

// A plugin broker's proxy port is remembered strictly: the first one comes
// from the stable range, and a remembered port that is taken still stops
// the broker rather than moving it.
func TestPluginBrokerPicksItsFirstPortFromTheStableRange(t *testing.T) {
	dir := t.TempDir()
	portFile := filepath.Join(dir, "mcp.port")
	broker := NewBroker(BrokerConfig{Socket: filepath.Join(dir, "mcp.sock"), PortFile: portFile, StrictPort: true})
	ctx, cancel := context.WithCancel(t.Context())
	if err := broker.serveProxy(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	<-broker.proxyDone
	requireStablePort(t, "plugin broker proxy", broker.proxyPort)
	if remembered := rememberedPortIn(portFile); remembered != broker.proxyPort {
		t.Fatalf("remembered %d, bound %d", remembered, broker.proxyPort)
	}

	taken, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(broker.proxyPort))
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	again := NewBroker(BrokerConfig{Socket: filepath.Join(dir, "again.sock"), PortFile: portFile, StrictPort: true})
	if err := again.serveProxy(t.Context()); err == nil || !strings.Contains(err.Error(), "plugin MCP port unavailable") {
		t.Fatalf("taken remembered port: %v", err)
	}
	if raw, _ := os.ReadFile(portFile); strings.TrimSpace(string(raw)) != strconv.Itoa(broker.proxyPort) {
		t.Fatalf("port record changed to %q", raw)
	}
}

// A plugin broker restarted after its proxy is done binds the remembered
// port again under StrictPort, so a done proxy has let go of its port. A
// broker stopped before its proxy server begins serving is the hard case.
func TestPluginBrokerProxyReleasesItsPortBeforeDone(t *testing.T) {
	for i := range 200 {
		broker := NewBroker(BrokerConfig{})
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := broker.serveProxy(ctx); err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
		<-broker.proxyDone
		again, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(broker.proxyPort))
		if err != nil {
			t.Fatalf("round %d: port still held once the proxy is done: %v", i, err)
		}
		again.Close()
	}
}

// Serve owns the proxy it started: when the socket cannot be served, Serve
// stops the proxy before it returns, even though its caller's context is
// still live.
func TestBrokerServeFailureReleasesTheProxyPort(t *testing.T) {
	dir := t.TempDir()
	notDir := filepath.Join(dir, "file")
	if err := os.WriteFile(notDir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	broker := NewBroker(BrokerConfig{Socket: filepath.Join(notDir, "mcp.sock")})
	if err := broker.Serve(t.Context()); err == nil {
		t.Fatal("served a socket under a regular file")
	}
	if broker.proxyPort == 0 {
		t.Fatal("proxy never listened")
	}
	again, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(broker.proxyPort))
	if err != nil {
		t.Fatalf("proxy port still held after Serve returned: %v", err)
	}
	again.Close()
}

// A plugin broker that fails to start has already let go of its remembered
// proxy port, so loading the runtime again binds that port under StrictPort;
// so has one that was dropped.
//
// The port is checked straight after the failed load. A proxy still running
// then would be stopped by another goroutine; with a single P that goroutine
// has not run yet when the check does, where with several it often has and
// the check would pass by luck.
func TestPluginRuntimeLoadRetriesOnItsPortAfterAFailedStart(t *testing.T) {
	procs := runtime.GOMAXPROCS(1)
	t.Cleanup(func() { runtime.GOMAXPROCS(procs) })
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer upstream.Close()
	s := NewServer(ServerConfig{Name: "worker", StateDir: t.TempDir()})
	selection := nodeRuntimeFixture(t, s, upstream.URL)
	pool := s.pluginRuntimePool()
	defer pool.Close()
	prepared, err := pool.Prepare(t.Context(), "runtime", selection, harness.Config{Command: "test"})
	if err != nil {
		t.Fatal(err)
	}
	socket, err := s.pluginStore().RuntimeSocket(t.Context(), prepared.Ref)
	if err != nil {
		t.Fatal(err)
	}
	port := 0
	released := func(round int, after string) {
		t.Helper()
		free, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil {
			t.Fatalf("round %d: proxy port still held after %s: %v", round, after, err)
		}
		free.Close()
	}
	for i := range 10 {
		// A non-empty directory where the socket goes: the broker's proxy
		// is up by the time its socket fails.
		if err := os.MkdirAll(filepath.Join(socket, "blocked"), 0o700); err != nil {
			t.Fatal(err)
		}
		_, err := pool.Load(t.Context(), prepared.Ref)
		if err == nil {
			t.Fatalf("round %d: loaded with its socket blocked", i)
		}
		if !strings.Contains(err.Error(), "mcp broker: listen") {
			t.Fatalf("round %d: load failed before its socket did: %v", i, err)
		}
		if port == 0 {
			port = rememberedPortIn(filepath.Join(s.pluginStore().RuntimeDir(prepared.Ref.ID), "mcp.port"))
			if port == 0 {
				t.Fatal("the failed load never started its proxy")
			}
		}
		released(i, "a failed load")
		if err := os.RemoveAll(socket); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Load(t.Context(), prepared.Ref); err != nil {
			t.Fatalf("round %d: load after a failed start: %v", i, err)
		}
		if err := pool.Drop(prepared.Ref.ID); err != nil {
			t.Fatal(err)
		}
		released(i, "the runtime was dropped")
	}
}
