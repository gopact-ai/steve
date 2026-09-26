//go:build unix

package node

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

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
// broker stopped before its proxy server begins serving is the hard case,
// and a single P makes that order common.
func TestPluginBrokerProxyReleasesItsPortBeforeDone(t *testing.T) {
	procs := runtime.GOMAXPROCS(1)
	t.Cleanup(func() { runtime.GOMAXPROCS(procs) })
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
