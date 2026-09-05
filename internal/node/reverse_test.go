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
	<-requestStarted
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
	resp, err = client.Post(endpoint, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.Status)
	}
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
