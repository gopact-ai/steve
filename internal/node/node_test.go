package node

import (
	"context"
	"errors"
	"github.com/gopact-ai/steve/internal/nodewire"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/permission"
)

// buildMockAgent compiles the ACP test agent so a node has something real to
// start. The point of these tests is that nothing about ACP notices the wire.
func buildMockAgent(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "mockagent")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent")
	cmd.Dir = "../.."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build mockagent: %v\n%s", err, out)
	}
	return bin
}

// startNode runs a real steve-node in-process and returns its address.
func startNode(t *testing.T, cfg ServerConfig) *Server {
	t.Helper()
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:0"
	}
	server := NewServer(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ready := make(chan struct{})
	go func() {
		close(ready)
		if err := server.Serve(ctx); err != nil {
			t.Errorf("node serve: %v", err)
		}
	}()
	<-ready
	// Serve binds before accepting; poll briefly rather than sleep blindly.
	for range 100 {
		if addr := server.Addr(); !strings.HasSuffix(addr, ":0") {
			return server
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("node never bound a port")
	return nil
}

// TestRemoteAgentRoundTrip is the acceptance test for the connection layer:
// an agent running under a node, driven by a host on the other side of a
// socket, answering an ordinary prompt.
func TestRemoteAgentRoundTrip(t *testing.T) {
	agent := buildMockAgent(t)
	server := startNode(t, ServerConfig{
		Name: "host-3", Token: "s3cret", StateDir: t.TempDir(),
		WorkspaceRoot: t.TempDir(),
		Harnesses:     map[string]HarnessSpec{"codex": {Command: agent, Models: []string{"gpt-5"}}},
		Capabilities:  []string{"gpu"},
	})

	registry := NewRegistry("hub-1", map[string]Config{
		"host-3": {Addr: server.Addr(), Token: "s3cret"},
	})
	t.Cleanup(registry.Close)

	broker, err := permission.New("auto")
	if err != nil {
		t.Fatal(err)
	}
	host := acphost.New(acphost.Config{
		Transport:  registry.Transport("host-3", "codex"),
		Permission: broker,
	})
	t.Cleanup(host.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sid, generation, err := host.OpenSession(ctx, "", acphost.SessionConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatalf("open session on remote node: %v", err)
	}
	out, _, err := host.Prompt(ctx, sid, generation, "hello world", nil)
	if err != nil {
		t.Fatalf("prompt on remote node: %v", err)
	}
	if out != "echo: hello world" {
		t.Fatalf("remote output = %q", out)
	}
}

// TestAdvertIsTheAuthority: the node reports what it can really run, and a
// placement the advert does not support is refused before a session opens.
func TestAdvertIsTheAuthority(t *testing.T) {
	server := startNode(t, ServerConfig{
		Name: "host-2", Token: "tok", StateDir: t.TempDir(),
		Harnesses: map[string]HarnessSpec{
			"codex":  {Command: buildMockAgent(t)},
			"absent": {Command: "definitely-not-installed-anywhere"},
		},
		Capabilities: []string{"internal-net"},
	})
	registry := NewRegistry("hub-1", map[string]Config{
		"host-2": {Addr: server.Addr(), Token: "tok"},
	})
	t.Cleanup(registry.Close)

	advert, err := registry.Advert(t.Context(), "host-2")
	if err != nil {
		t.Fatal(err)
	}
	if advert.Node != "host-2" || advert.MCPPort == 0 {
		t.Fatalf("advert = %+v (want node name and a reverse MCP port)", advert)
	}
	byID := map[string]string{}
	for _, h := range advert.Harnesses {
		byID[h.ID] = h.Missing
	}
	if byID["codex"] != "" {
		t.Errorf("codex reported missing: %q", byID["codex"])
	}
	if !strings.Contains(byID["absent"], "PATH") {
		t.Errorf("absent harness should say why it is unusable, got %q", byID["absent"])
	}

	// A harness the node cannot run is refused at Start, naming the reason,
	// rather than opening a session that dies on first prompt.
	_, err = registry.Transport("host-2", "absent").Start(t.Context())
	if err == nil || !strings.Contains(err.Error(), "PATH") {
		t.Fatalf("Start on a missing harness = %v, want a PATH complaint", err)
	}
}

func TestBadTokenIsRefused(t *testing.T) {
	server := startNode(t, ServerConfig{
		Name: "host-2", Token: "right", StateDir: t.TempDir(),
		Harnesses: map[string]HarnessSpec{"codex": {Command: "echo"}},
	})
	registry := NewRegistry("hub-1", map[string]Config{
		"host-2": {Addr: server.Addr(), Token: "wrong"},
	})
	t.Cleanup(registry.Close)
	if _, err := registry.Advert(t.Context(), "host-2"); err == nil {
		t.Fatal("a wrong token connected")
	}
	statuses := registry.Statuses()
	if len(statuses) != 1 || statuses[0].Up || statuses[0].LastError == "" {
		t.Fatalf("status should record the refusal: %+v", statuses)
	}
}

// TestUnreachableNodeFailsFast: a node that is down must produce a named
// error, not a hung turn.
func TestUnreachableNodeFailsFast(t *testing.T) {
	registry := NewRegistry("hub-1", map[string]Config{
		// 127.0.0.1:1 is reliably closed.
		"gone": {Addr: "127.0.0.1:1", Token: "t", DialTimeout: 2 * time.Second},
	})
	t.Cleanup(registry.Close)
	start := time.Now()
	_, err := registry.Transport("gone", "codex").Start(t.Context())
	if err == nil {
		t.Fatal("expected a dial failure")
	}
	if !strings.Contains(err.Error(), "gone") {
		t.Errorf("error should name the node: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("dial took %s, should fail fast", elapsed)
	}
}

// TestReverseMCPTunnel is the other half of the connection layer: an agent on
// the node calls a loopback port on its *own* machine, and the request comes
// out at the hub's loopback messaging server. This is what lets a remote
// agent send milestone cards without the hub ever exposing that server.
func TestReverseMCPTunnel(t *testing.T) {
	// Stand in for agentmcp: a loopback HTTP server only the hub can reach.
	var gotAuth, gotBody string
	hubServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(hubServer.Close)
	hubAddr := strings.TrimPrefix(hubServer.URL, "http://")

	server := startNode(t, ServerConfig{
		Name: "host-3", Token: "tok", StateDir: t.TempDir(),
		Harnesses: map[string]HarnessSpec{"codex": {Command: "echo"}},
	})
	registry := NewRegistry("hub-1", map[string]Config{
		"host-3": {Addr: server.Addr(), Token: "tok"},
	})
	t.Cleanup(registry.Close)
	registry.SetMCPDialer(func(ctx context.Context) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "tcp", hubAddr)
	})

	endpoint, err := registry.MCPEndpoint(t.Context(), "host-3")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(endpoint, "http://127.0.0.1:") {
		t.Fatalf("endpoint = %q, want a loopback address on the node", endpoint)
	}

	// Call it the way an agent on that machine would.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint,
		strings.NewReader(`{"jsonrpc":"2.0","method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer session-token")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("call through the reverse tunnel: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != `{"ok":true}` {
		t.Fatalf("response through the tunnel = %q", body)
	}
	if gotAuth != "Bearer session-token" {
		t.Errorf("per-session token did not survive the tunnel: %q", gotAuth)
	}
	if !strings.Contains(gotBody, "tools/list") {
		t.Errorf("request body did not survive the tunnel: %q", gotBody)
	}
}

// Verification commands run on the node, in the node's filesystem, and their
// exit status comes back as a typed failure rather than a guess.
func TestExecRunsCommandsOnTheNode(t *testing.T) {
	workspace := t.TempDir()
	server := startNode(t, ServerConfig{
		Name: "host-3", Token: "tok", StateDir: t.TempDir(), WorkspaceRoot: workspace,
		Harnesses: map[string]HarnessSpec{"codex": {Command: "echo"}},
	})
	registry := NewRegistry("hub-1", map[string]Config{
		"host-3": {Addr: server.Addr(), Token: "tok"},
	})
	t.Cleanup(registry.Close)

	// Output comes back; the command ran in the workspace, not the hub's cwd.
	out, err := registry.Exec(t.Context(), "host-3", "", "pwd; echo hello from the node")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, workspace) || !strings.Contains(out, "hello from the node") {
		t.Fatalf("output = %q", out)
	}

	// A failing check is an ExitError carrying the output, so the plan can
	// record why rather than just that.
	_, err = registry.Exec(t.Context(), "host-3", "", "echo tests failed >&2; exit 3")
	var exit ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("err = %T %v, want ExitError", err, err)
	}
	if exit.Code != 3 || !strings.Contains(exit.Output, "tests failed") {
		t.Fatalf("exit = %+v", exit)
	}

	// The hub runs its own checks locally through the same call.
	local, err := registry.Exec(t.Context(), "", t.TempDir(), "echo local")
	if err != nil || !strings.Contains(local, "local") {
		t.Fatalf("local exec = %q, %v", local, err)
	}
}

// A harness that appears after the handshake is seen when the hub asks the
// node to check itself again — without a reconnect, so nothing running on
// the node notices.
func TestRefreshSeesARepairedHarness(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "later-installed")
	server := startNode(t, ServerConfig{
		Name: "host-3", Token: "tok", StateDir: t.TempDir(),
		Harnesses: map[string]HarnessSpec{
			"codex": {Command: buildMockAgent(t)},
			"later": {Command: bin},
		},
	})
	registry := NewRegistry("hub-1", map[string]Config{"host-3": {Addr: server.Addr(), Token: "tok"}})
	t.Cleanup(registry.Close)

	missing := func(adv nodewire.Advert) string {
		for _, h := range adv.Harnesses {
			if h.ID == "later" {
				return h.Missing
			}
		}
		return "not listed"
	}
	first, err := registry.Advert(t.Context(), "host-3")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(missing(first), "PATH") {
		t.Fatalf("before install: %q", missing(first))
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Nothing changes until asked: the advert is a statement, not a poll.
	if cached, _ := registry.Advert(t.Context(), "host-3"); missing(cached) == "" {
		t.Fatal("advert changed without a refresh")
	}
	fresh, err := registry.Refresh(t.Context(), "host-3")
	if err != nil {
		t.Fatal(err)
	}
	if missing(fresh) != "" || fresh.Node != "host-3" {
		t.Fatalf("after refresh: %+v", fresh)
	}
	// The refreshed advert is what the roster reads from now on.
	for _, s := range registry.Statuses() {
		if s.Name == "host-3" && missing(s.Advert) != "" {
			t.Fatalf("status still says %q", missing(s.Advert))
		}
	}
	if again, _ := registry.Advert(t.Context(), "host-3"); missing(again) != "" {
		t.Fatal("connection kept the old advert")
	}
}
