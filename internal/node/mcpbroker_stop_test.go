//go:build unix

package node

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/harness"
)

// failingListener fails Accept once fail is closed, the way a listener
// does when the process has run out of descriptors.
type failingListener struct {
	net.Listener
	fail   <-chan struct{}
	once   sync.Once
	closed chan struct{}
}

func failAccepting(l net.Listener, fail <-chan struct{}) *failingListener {
	return &failingListener{Listener: l, fail: fail, closed: make(chan struct{})}
}

func (l *failingListener) Accept() (net.Conn, error) {
	select {
	case <-l.fail:
		return nil, fmt.Errorf("accept: %w", syscall.EMFILE)
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *failingListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return l.Listener.Close()
}

// A broker that can no longer accept has let go of its socket by the time
// Serve returns: a launcher that connects afterwards is refused at once
// instead of waiting on a socket nobody accepts on.
func TestBrokerClosesItsSocketWhenAcceptFails(t *testing.T) {
	fail := make(chan struct{})
	close(fail)
	testHookSocketListener = func(l net.Listener) net.Listener { return failAccepting(l, fail) }
	t.Cleanup(func() { testHookSocketListener = nil })
	socket := filepath.Join(t.TempDir(), "mcp.sock")
	err := NewBroker(BrokerConfig{Socket: socket}).Serve(t.Context())
	if !errors.Is(err, syscall.EMFILE) {
		t.Fatalf("Serve returned %v, want the accept failure", err)
	}
	if conn, err := net.Dial("unix", socket); err == nil {
		conn.Close()
		t.Fatal("the socket still takes connections after Serve returned")
	}
}

// A broker whose Serve has returned refuses every bind and says why.
// Handing one out would give a session an HTTP route to a proxy that is
// gone, or a launcher for a socket nobody accepts on.
func TestAStoppedBrokerRefusesBinds(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer upstream.Close()
	servers := map[string]MCPSpec{"api": {Type: "http", URL: upstream.URL}, "tool": {Type: "stdio", Command: "true"}}
	refused := func(t *testing.T, broker *Broker, reason string) {
		t.Helper()
		for id := range servers {
			binding, err := broker.Bind(id, "attempt", "harness")
			if err == nil {
				t.Fatalf("bound %s on a stopped broker: %+v", id, binding)
			}
			if !strings.Contains(err.Error(), reason) {
				t.Fatalf("bind %s refused without saying %q: %v", id, reason, err)
			}
		}
	}
	t.Run("its socket failed", func(t *testing.T) {
		notDir := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(notDir, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		broker := NewBroker(BrokerConfig{Socket: filepath.Join(notDir, "mcp.sock"), MCPServers: servers})
		err := broker.Serve(t.Context())
		if err == nil {
			t.Fatal("served a socket under a regular file")
		}
		refused(t, broker, err.Error())
	})
	t.Run("its context ended", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		broker := NewBroker(BrokerConfig{Socket: filepath.Join(t.TempDir(), "mcp.sock"), MCPServers: servers})
		if err := broker.Serve(ctx); err != nil {
			t.Fatal(err)
		}
		refused(t, broker, context.Canceled.Error())
	})
}

// A plugin runtime whose broker stops on its own is not served from the
// cache: the next load starts the broker again on the remembered port, so
// the route sessions already hold reaches the server again.
func TestPluginRuntimeLoadRestartsABrokerThatStoppedOnItsOwn(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	defer upstream.Close()
	fail := make(chan struct{})
	var wrapped atomic.Bool
	testHookSocketListener = func(l net.Listener) net.Listener {
		if wrapped.Swap(true) {
			return l
		}
		return failAccepting(l, fail)
	}
	t.Cleanup(func() { testHookSocketListener = nil })
	s := NewServer(ServerConfig{Name: "worker", StateDir: t.TempDir()})
	selection := nodeRuntimeFixture(t, s, upstream.URL)
	pool := s.pluginRuntimePool()
	defer pool.Close()
	prepared, err := pool.Prepare(t.Context(), "runtime", selection, harness.Config{Command: "test"})
	if err != nil {
		t.Fatal(err)
	}
	reaches := func(url string) bool {
		response, err := http.Get(url)
		if err != nil {
			return false
		}
		response.Body.Close()
		return response.StatusCode == http.StatusTeapot
	}
	loaded, err := pool.Load(t.Context(), prepared.Ref)
	if err != nil {
		t.Fatal(err)
	}
	route := loaded.Servers[0].URL
	if !reaches(route) {
		t.Fatal("the runtime's route does not reach its server")
	}
	close(fail)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		again, err := pool.Load(t.Context(), prepared.Ref)
		if err != nil {
			t.Fatal(err)
		}
		if again.Servers[0].URL != route {
			t.Fatalf("route moved from %s to %s", route, again.Servers[0].URL)
		}
		if reaches(route) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("a runtime whose broker stopped is still served from the cache; its route is dead")
		}
	}
}
