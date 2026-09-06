package node

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/nodewire"
)

func TestMCPListenerStaysPutAndReturnsRetryableErrorDuringOutage(t *testing.T) {
	requestStarted := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.URL.Query().Get("hang") != "" {
			requestStarted <- struct{}{}
			<-r.Context().Done()
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(upstream.Close)
	s := startNode(t, ServerConfig{Name: "n", Token: "token", StateDir: t.TempDir(), Harnesses: map[string]HarnessSpec{"cat": {Command: "/bin/cat"}}})
	r := NewRegistry("hub", map[string]Config{"n": {Addr: s.Addr(), Token: "token"}})
	t.Cleanup(r.Close)
	r.SetMCPDialer(func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", strings.TrimPrefix(upstream.URL, "http://"))
	})
	c, err := r.connect(t.Context(), "n")
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := r.MCPEndpoint(t.Context(), "n")
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 2 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	waitTunnel(t, endpoint)
	result := make(chan *http.Response, 1)
	failure := make(chan error, 1)
	go func() {
		resp, err := client.Post(endpoint+"?hang=1", "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call"}`))
		if err != nil {
			failure <- err
		} else {
			result <- resp
		}
	}()
	// Bound the wait. This was a plain receive, so a request that never
	// reached upstream left the test blocked until the package's ten-minute
	// alarm — one flake wedging the whole run.
	select {
	case <-requestStarted:
	case err := <-failure:
		t.Fatalf("the request never reached upstream: %v", err)
	case resp := <-result:
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("the request never reached upstream: %d %s", resp.StatusCode, body)
	case <-time.After(10 * time.Second):
		t.Fatal("the request never reached upstream")
	}
	_ = c.mux.Close()
	select {
	case resp := <-result:
		checkUnreachable(t, resp)
	case err := <-failure:
		t.Fatal(err)
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight MCP hung after disconnect")
	}
	resp, err := client.Post(endpoint, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	checkUnreachable(t, resp)
	if _, err := r.connect(t.Context(), "n"); err != nil {
		t.Fatal(err)
	}
	after, err := r.MCPEndpoint(t.Context(), "n")
	if err != nil || after != endpoint {
		t.Fatalf("endpoint changed: %q -> %q (%v)", endpoint, after, err)
	}
	// Recovery is asynchronous on the node's side too: connect returns
	// when the hub has the connection, and the node records its end of it
	// in its own goroutine. What the endpoint promises is that it comes
	// back on the same port, not that it is back the instant the hub
	// says so — until then it answers the retryable error, correctly.
	waitTunnel(t, endpoint)
}

func checkUnreachable(t *testing.T, r *http.Response) {
	t.Helper()
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	var rpc struct {
		JSONRPC string `json:"jsonrpc"`
		Error   struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &rpc); err != nil {
		t.Fatal(string(body), err)
	}
	if r.StatusCode != 503 || rpc.JSONRPC != "2.0" || rpc.Error.Message != "hub unreachable, retry later" {
		t.Fatalf("%s: %s", r.Status, body)
	}
}

func TestFaultDropsHubOnlyOnceAfterFirstACPStream(t *testing.T) {
	m := newMemoryNode(t, "/bin/cat")
	cfg := m.s.conf()
	cfg.FaultDropAfter = 20 * time.Millisecond
	m.s.cfg.Store(&cfg)
	first := m.connection(t)
	m.s.injectDrop(m.ctx, first)
	select {
	case <-first.Done():
	case <-time.After(time.Second):
		t.Fatal("fault did not fire")
	}
	second := m.connection(t)
	m.s.injectDrop(m.ctx, second)
	select {
	case <-second.Done():
		t.Fatal("fault fired twice")
	case <-time.After(60 * time.Millisecond):
	}
}

func TestLegacyHubAndNodeKeepOriginalStreams(t *testing.T) {
	m := newMemoryNode(t, "/bin/cat")
	r := memoryRegistry(t)
	c := connectMemory(t, m, r)
	adv := c.getAdvert()
	adv.Features = nil
	c.setAdvert(adv)
	p, err := r.Transport("n", "cat").Start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Kill()
	legacy, ok := p.(legacyRemoteProcess)
	if !ok || legacy.stream.Request().Stream != "" {
		t.Fatalf("legacy transport = %T", p)
	}
	_, _ = io.WriteString(p.Stdin(), "raw without newline")
	buf := make([]byte, len("raw without newline"))
	if _, err := io.ReadFull(p.Stdout(), buf); err != nil || string(buf) != "raw without newline" {
		t.Fatal(string(buf), err)
	}
	_ = c.mux.Close()
	_, err = p.Stdout().Read(buf)
	if err == nil {
		t.Fatal("legacy stream pretended to resume")
	}
	// An old hub omits the four fields, which the new server treats exactly
	// as above. No ResumeAck or sideband acknowledgement enters its stream.
	if legacy.stream.Request().Kind != nodewire.StreamACP {
		t.Fatal("wrong kind")
	}
}

// waitTunnel blocks until the node's reverse channel is serving. Wiring the
// MCP dialer starts it in a goroutine (conn.go), so until it is up the node
// answers the documented 503 with Retry-After — correct behaviour, and not
// something a test that needs the tunnel should race against. Nothing tells
// a caller when the channel is ready, so polling is what a caller can do,
// and it is what these tests do.
func waitTunnel(t *testing.T, endpoint string) {
	t.Helper()
	client := &http.Client{Timeout: 10 * time.Second}
	defer client.CloseIdleConnections()
	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, err := client.Post(endpoint, "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatalf("reverse channel: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the reverse channel never came up: %s", body)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The reverse listener is bound during the handshake, before the hub
// connection is recorded (serve.go), so a tool call can arrive in that
// window. It waits for the hub rather than reporting it unreachable.
func TestTheFirstCallWaitsForTheHubToAttach(t *testing.T) {
	s := &Server{}
	got := make(chan error, 1)
	go func() {
		_, err := s.awaitHub(t.Context())
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("answered before any hub existed: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	s.mu.Lock()
	s.hubMux = &nodewire.Mux{}
	s.hubSeen = true
	if s.hubWaiters != nil {
		close(s.hubWaiters)
		s.hubWaiters = nil
	}
	s.mu.Unlock()
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("the wait ended badly once the hub was there: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("attaching a hub did not release the waiting call")
	}
}

// An outage is not a cold start. Once a hub has been here, its absence is
// reported at once: it reconnects on its own, and a caller told to retry
// can do something with that, while a caller left waiting cannot.
func TestAnOutageIsReportedAtOnce(t *testing.T) {
	s := &Server{}
	s.hubSeen = true
	start := time.Now()
	if _, err := s.awaitHub(t.Context()); err == nil {
		t.Fatal("a node whose hub is gone reported one")
	}
	if waited := time.Since(start); waited > time.Second {
		t.Fatalf("waited %s for a hub that had already been and gone", waited)
	}
}
