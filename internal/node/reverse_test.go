package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/nodewire"
)

func TestReverseMCPWaitsForToolResponseAndPropagatesCancellation(t *testing.T) {
	started := make(chan struct{}, 1)
	canceled := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		switch r.URL.Query().Get("wait") {
		case "tool":
			// Delegation waits 20 seconds and await may wait 50 seconds before
			// writing response headers. Neither is an unreachable hub.
			select {
			case <-time.After(16 * time.Second):
			case <-r.Context().Done():
				return
			}
		case "cancel":
			started <- struct{}{}
			<-r.Context().Done()
			canceled <- struct{}{}
			return
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"state":"running"}}`))
	}))
	t.Cleanup(upstream.Close)
	s := startNode(t, ServerConfig{Name: "n", Token: "token", StateDir: t.TempDir(), Harnesses: map[string]HarnessSpec{"cat": {Command: "/bin/cat"}}})
	r := NewRegistry("hub", map[string]Config{"n": {Addr: s.Addr(), Token: "token"}})
	t.Cleanup(r.Close)
	r.SetMCPDialer(func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(upstream.URL, "http://"))
	})
	endpoint, err := r.MCPEndpoint(t.Context(), "n")
	if err != nil {
		t.Fatal(err)
	}
	waitTunnel(t, endpoint)
	client := &http.Client{Timeout: 25 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	t.Run("long tool call", func(t *testing.T) {
		resp, err := client.Post(endpoint+"?wait=tool", "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call"}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"state":"running"`) {
			t.Fatalf("long tool call: status=%d body=%s err=%v", resp.StatusCode, body, err)
		}
	})
	t.Run("caller cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"?wait=cancel", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		result := make(chan error, 1)
		go func() {
			resp, err := client.Do(req)
			if resp != nil {
				resp.Body.Close()
			}
			result <- err
		}()
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("request did not reach upstream")
		}
		cancel()
		select {
		case err := <-result:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("caller cancellation = %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("caller cancellation did not finish the request")
		}
		select {
		case <-canceled:
		case <-time.After(5 * time.Second):
			t.Fatal("caller cancellation did not release upstream")
		}
	})
}

func TestMCPListenerStaysPutAndReturnsRetryableErrorDuringOutage(t *testing.T) {
	requestStarted := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.URL.Query().Get("hang") != "" {
			requestStarted <- struct{}{}
			<-r.Context().Done()
			// Returning normally can synthesize an empty HTTP 200 while
			// the disconnected tunnel is still closing. This fixture must
			// abort its response so the proxy observes the outage.
			panic(http.ErrAbortHandler)
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

// v1Node answers a handshake the way a build that speaks only protocol v1
// does: it settles on v1 when the hub's range includes it and refuses
// with both ranges otherwise. Its advert lists every feature a v1 build
// declared, the process journal included.
func v1Node(t *testing.T, socket net.Conn) {
	t.Helper()
	defer socket.Close()
	f, err := nodewire.ReadFrame(socket)
	if err != nil {
		return
	}
	var hello nodewire.Hello
	if err := json.Unmarshal(f.Payload, &hello); err != nil {
		t.Error(err)
		return
	}
	lo, hi := hello.ProtocolMin, hello.ProtocolMax
	if hi == 0 {
		lo, hi = hello.Version, hello.Version
	}
	advert := map[string]any{"version": 1, "node": "old", "harnesses": []any{},
		"features": []string{"plugin_runtimes.v1", "plugin_packages.v1", "manifest.v1", "execution_admission.v1", "skill_bundle.v1", "node_mcp_binding.v1", "node_config.v1", "node_config_revision.v1", "inspect.v1", "mcp_probe.v1", "own_skills.v1", "process_journal.v1", "artifact_ops.v1", "file_ops.v1"}}
	if lo > 1 || hi < 1 {
		advert = map[string]any{"version": 1, "harnesses": nil, "refused": fmt.Sprintf("hub speaks v%d–v%d, node speaks v1–v1", lo, hi)}
	}
	payload, _ := json.Marshal(advert)
	_ = nodewire.WriteFrame(socket, nodewire.Frame{Kind: nodewire.KindOpen, Payload: payload})
}

// A node on protocol v1 is refused at the handshake, whatever features it
// lists, and the hub says once which node it was, which versions met and
// how to fix it.
func TestHubRefusesAProtocolV1Node(t *testing.T) {
	r := NewRegistry("hub", map[string]Config{"old": {Addr: "old.example:7701", Token: "t", DialContext: func(context.Context, string) (net.Conn, error) {
		hub, node := net.Pipe()
		go v1Node(t, node)
		return hub, nil
	}}})
	t.Cleanup(r.Close)
	_, err := r.connect(t.Context(), "old")
	if !errors.Is(err, nodewire.ErrVersionMismatch) {
		t.Fatalf("connect = %v, want a version mismatch", err)
	}
	if n := strings.Count(err.Error(), `"old"`); n != 1 {
		t.Fatalf("connect = %v, names the node %d times, want once", err, n)
	}
	for _, want := range []string{"node speaks v1–v1", "upgrade steve on that machine", "over SSH"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("connect = %v, want it to say %q", err, want)
		}
	}
}

// A node refused for its protocol version is listed with the versions that
// met, so a reader can tell it from a node that is merely down; a failure
// for any other reason, and the next successful connection, clear them.
func TestStatusesKeepWhyANodeWasRefusedForItsVersion(t *testing.T) {
	server := startNode(t, ServerConfig{Name: "old", Token: "t", StateDir: t.TempDir()})
	var mode atomic.Value
	r := NewRegistry("hub", map[string]Config{"old": {Addr: server.Addr(), Token: "t", DialContext: func(ctx context.Context, _ string) (net.Conn, error) {
		switch mode.Load() {
		case "v1":
			hub, node := net.Pipe()
			go v1Node(t, node)
			return hub, nil
		case "down":
			return nil, errors.New("connection refused")
		}
		var d net.Dialer
		return d.DialContext(ctx, "tcp", server.Addr())
	}}})
	t.Cleanup(r.Close)
	status := func() Status {
		t.Helper()
		all := r.Statuses()
		if len(all) != 1 {
			t.Fatalf("statuses = %+v", all)
		}
		return all[0]
	}
	want := nodewire.VersionMismatch{Node: 1, HubMin: nodewire.ProtocolMin, HubMax: nodewire.ProtocolVersion}

	mode.Store("v1")
	if _, err := r.connect(t.Context(), "old"); err == nil {
		t.Fatal("a v1 node connected")
	}
	got := status()
	if got.Up || got.Mismatch == nil || got.Mismatch.Node != want.Node || got.Mismatch.HubMin != want.HubMin || got.Mismatch.HubMax != want.HubMax {
		t.Fatalf("refused node = %+v (mismatch %+v), want down with v1 against v%d–v%d", got, got.Mismatch, want.HubMin, want.HubMax)
	}
	if !strings.Contains(got.LastError, "node speaks v1–v1") {
		t.Fatalf("last error = %q, want the versions named", got.LastError)
	}

	mode.Store("down")
	if _, err := r.connect(t.Context(), "old"); err == nil {
		t.Fatal("an unreachable node connected")
	}
	if got := status(); got.Up || got.Mismatch != nil {
		t.Fatalf("unreachable node = %+v (mismatch %+v), want down with no version mismatch", got, got.Mismatch)
	}

	mode.Store("v1")
	_, _ = r.connect(t.Context(), "old")
	mode.Store("v2")
	if _, err := r.connect(t.Context(), "old"); err != nil {
		t.Fatalf("connect v2 node: %v", err)
	}
	if got := status(); !got.Up || got.Mismatch != nil {
		t.Fatalf("upgraded node = %+v (mismatch %+v), want up with no version mismatch", got, got.Mismatch)
	}
}

// A dial that fails after its machine was removed or reconfigured records
// nothing: the machine is listed under its current configuration as not
// contacted yet, not with the refusal the old configuration earned.
func TestAStaleDialFailureIsNotRecorded(t *testing.T) {
	for name, change := range map[string]func(r *Registry, cfg Config){
		"removed and added again": func(r *Registry, cfg Config) { r.Remove("old"); r.Add("old", cfg) },
		"reconfigured":            func(r *Registry, cfg Config) { r.Add("old", cfg) },
	} {
		t.Run(name, func(t *testing.T) {
			started, release, parked := make(chan struct{}), make(chan struct{}), make(chan struct{})
			t.Cleanup(func() { close(parked) })
			r := NewRegistry("hub", map[string]Config{"old": {Addr: "old.example:7701", Token: "t", DialContext: func(context.Context, string) (net.Conn, error) {
				close(started)
				<-release
				hub, node := net.Pipe()
				go v1Node(t, node)
				return hub, nil
			}}})
			t.Cleanup(r.Close)
			done := make(chan error, 1)
			go func() {
				_, err := r.connect(t.Context(), "old")
				done <- err
			}()
			<-started
			// The new configuration's dial waits for the old one, then parks
			// until the test ends, so only the old dial can record anything.
			change(r, Config{Addr: "new.example:7701", Token: "t", DialContext: func(ctx context.Context, _ string) (net.Conn, error) {
				select {
				case <-parked:
				case <-ctx.Done():
				}
				return nil, errors.New("parked")
			}})
			close(release)
			if err := <-done; !errors.Is(err, nodewire.ErrVersionMismatch) {
				t.Fatalf("old dial = %v, want the version refusal", err)
			}
			all := r.Statuses()
			if len(all) != 1 {
				t.Fatalf("statuses = %+v", all)
			}
			if got := all[0]; got.Addr != "new.example:7701" || got.LastError != "not contacted yet" || got.Mismatch != nil {
				t.Fatalf("status = %+v (mismatch %+v), want the new address not contacted yet", got, got.Mismatch)
			}
		})
	}
}

// A node refused for speaking only versions newer than the hub's is not
// told to upgrade: the hub is the one behind.
func TestHubRefusedByANewerNodeAdvisesItsOwnUpgrade(t *testing.T) {
	newer := nodewire.ProtocolVersion + 1
	cfg := Config{Token: "t", DialContext: func(context.Context, string) (net.Conn, error) {
		hub, node := net.Pipe()
		go func() {
			defer node.Close()
			if _, err := nodewire.ReadFrame(node); err != nil {
				return
			}
			payload, _ := json.Marshal(nodewire.Advert{Version: newer, Refused: fmt.Sprintf("hub speaks v%d–v%d, node speaks v%d–v%d", nodewire.ProtocolMin, nodewire.ProtocolVersion, newer, newer)})
			_ = nodewire.WriteFrame(node, nodewire.Frame{Kind: nodewire.KindOpen, Payload: payload})
		}()
		return hub, nil
	}}
	c, err := dial(t.Context(), "new", "hub", cfg, nil)
	if c != nil {
		c.close()
	}
	if !errors.Is(err, nodewire.ErrVersionMismatch) || !strings.Contains(err.Error(), fmt.Sprintf("upgrade this hub to a build that speaks v%d", newer)) || strings.Contains(err.Error(), "over SSH") {
		t.Fatalf("dial = %v, want the hub's own upgrade advised", err)
	}
}

// A node that answers with a version outside the hub's range, instead of
// refusing, is refused by the hub with the same advice.
func TestHubRefusesAnAdvertOnAnOlderProtocol(t *testing.T) {
	cfg := Config{Token: "t", DialContext: func(context.Context, string) (net.Conn, error) {
		hub, node := net.Pipe()
		go func() {
			defer node.Close()
			if _, err := nodewire.ReadFrame(node); err != nil {
				return
			}
			payload, _ := json.Marshal(nodewire.Advert{Version: 1, Node: "old"})
			_ = nodewire.WriteFrame(node, nodewire.Frame{Kind: nodewire.KindOpen, Payload: payload})
		}()
		return hub, nil
	}}
	c, err := dial(t.Context(), "old", "hub", cfg, nil)
	if c != nil {
		c.close()
	}
	if !errors.Is(err, nodewire.ErrVersionMismatch) || !strings.Contains(err.Error(), "node speaks v1") || !strings.Contains(err.Error(), "over SSH") {
		t.Fatalf("dial = %v, want a version mismatch naming v1 and the SSH upgrade", err)
	}
}

func TestNodeRefusesAnAgentStreamWithoutAnID(t *testing.T) {
	m := newMemoryNode(t, "/bin/cat")
	stream, err := m.connection(t).Open(nodewire.OpenRequest{Kind: nodewire.StreamACP, Harness: "cat"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-stream.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the node started an agent for a stream it cannot resume")
	}
	if err := stream.Err(); err == nil || !strings.Contains(err.Error(), "invalid stream id") {
		t.Fatalf("stream ended with %v, want invalid stream id", err)
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
