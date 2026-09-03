// Package mesh is the three-host acceptance suite. It talks to real
// steve-node processes over a real network, so it is opt-in: set
// STEVE_MESH_E2E=1 and point the two node addresses at live nodes.
//
//	STEVE_MESH_E2E=1 go test ./e2e/mesh/ -v
//
// Scenario numbers refer to SCENARIOS.md in this directory.
package mesh

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/permission"
)

const (
	nodeA = "node-a"
	nodeB = "node-b"
)

func addrA() string  { return env("STEVE_MESH_NODE_A", "10.37.124.132:7701") }
func addrB() string  { return env("STEVE_MESH_NODE_B", "10.37.97.2:7701") }
func tokenA() string { return env("STEVE_MESH_TOKEN_A", "e2e-mesh-token-a") }
func tokenB() string { return env("STEVE_MESH_TOKEN_B", "e2e-mesh-token-b") }

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func requireMesh(t *testing.T) {
	t.Helper()
	if os.Getenv("STEVE_MESH_E2E") == "" {
		t.Skip("set STEVE_MESH_E2E=1 to run the three-host suite")
	}
}

func registry(t *testing.T) *node.Registry {
	t.Helper()
	reg := node.NewRegistry("hub-e2e", map[string]node.Config{
		nodeA: {Addr: addrA(), Token: tokenA(), DialTimeout: 10 * time.Second},
		nodeB: {Addr: addrB(), Token: tokenB(), DialTimeout: 10 * time.Second},
	})
	t.Cleanup(reg.Close)
	return reg
}

// A1: every configured node answers, and says honestly what it can run.
func TestA1NodesRegister(t *testing.T) {
	requireMesh(t)
	reg := registry(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	statuses := reg.Probe(ctx)
	if len(statuses) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(statuses))
	}
	caps := map[string][]string{}
	for _, s := range statuses {
		if !s.Up {
			t.Fatalf("node %s (%s) down: %s", s.Name, s.Addr, s.LastError)
		}
		t.Logf("%s: %s/%s caps=%v workspace=%s mcp_port=%d",
			s.Name, s.Advert.OS, s.Advert.Arch, s.Advert.Capabilities,
			s.Advert.WorkspaceRoot, s.Advert.MCPPort)
		if s.Advert.Node != s.Name {
			t.Errorf("node reported itself as %q, configured as %q", s.Advert.Node, s.Name)
		}
		if s.Advert.MCPPort == 0 {
			t.Errorf("%s advertised no reverse messaging port", s.Name)
		}
		caps[s.Name] = s.Advert.Capabilities
		for _, h := range s.Advert.Harnesses {
			t.Logf("  harness %s models=%v missing=%q", h.ID, h.Models, h.Missing)
		}
	}

	// The two nodes must be distinguishable by capability — that is what
	// makes placement a real decision rather than a coin toss.
	if !has(caps[nodeA], "gpu") {
		t.Errorf("%s should advertise gpu, got %v", nodeA, caps[nodeA])
	}
	if !has(caps[nodeB], "internal-net") {
		t.Errorf("%s should advertise internal-net, got %v", nodeB, caps[nodeB])
	}
}

// A1b: a harness whose binary is absent is reported as broken, not omitted.
// A roster that hides what is broken sends the hub hunting for a ghost. The
// broken harness lives on a steve-node started here for the purpose, so the
// fleet's own machines carry no fixtures.
func TestA1MissingHarnessIsReportedNotHidden(t *testing.T) {
	requireMesh(t)
	broken := node.NewServer(node.ServerConfig{
		Name: "fixture", Token: "fixture-token", StateDir: t.TempDir(), Listen: "127.0.0.1:0",
		Harnesses: map[string]node.HarnessSpec{"absent": {Command: "definitely-not-installed"}},
	})
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go func() { _ = broken.Serve(ctx) }()
	for range 100 {
		if addr := broken.Addr(); addr != "" && !strings.HasSuffix(addr, ":0") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	reg := node.NewRegistry("hub-e2e", map[string]node.Config{"fixture": {Addr: broken.Addr(), Token: "fixture-token"}})
	t.Cleanup(reg.Close)
	advert, err := reg.Advert(t.Context(), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	var sawAbsent bool
	for _, h := range advert.Harnesses {
		if h.ID != "absent" {
			continue
		}
		sawAbsent = true
		if h.Missing == "" {
			t.Error("a harness that is not installed reported itself as fine")
		}
		if !strings.Contains(h.Missing, "PATH") {
			t.Errorf("missing reason should name the cause, got %q", h.Missing)
		}
	}
	if !sawAbsent {
		t.Error("the unusable harness was omitted from the advert instead of flagged")
	}
	// And starting it is refused with that reason, before any session opens.
	if _, err := reg.Transport("fixture", "absent").Start(t.Context()); err == nil {
		t.Error("starting an unavailable harness succeeded")
	} else if !strings.Contains(err.Error(), "PATH") {
		t.Errorf("refusal should carry the reason, got %v", err)
	}
}

func has(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// A2: an agent runs on the node and answers an ordinary prompt. The host
// driving it is on another machine and cannot tell the difference.
func TestA2RemoteSessionRoundTrip(t *testing.T) {
	requireMesh(t)
	reg := registry(t)
	for _, nodeName := range []string{nodeA, nodeB} {
		t.Run(nodeName, func(t *testing.T) {
			broker, err := permission.New("auto")
			if err != nil {
				t.Fatal(err)
			}
			host := acphost.New(acphost.Config{
				Transport: reg.Transport(nodeName, "mock"), Permission: broker,
			})
			t.Cleanup(host.Stop)
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			sid, generation, err := host.OpenSession(ctx, "", acphost.SessionConfig{Workdir: "/home/pengxiang.lpx/steve-work"})
			if err != nil {
				t.Fatalf("open session on %s: %v", nodeName, err)
			}
			out, _, err := host.Prompt(ctx, sid, generation, "hello "+nodeName, nil)
			if err != nil {
				t.Fatalf("prompt on %s: %v", nodeName, err)
			}
			if out != "echo: hello "+nodeName {
				t.Fatalf("remote answer = %q", out)
			}
			t.Logf("%s answered: %q", nodeName, out)
		})
	}
}

// A2b: the agent process really is on the node, not quietly on the hub.
func TestA2ProcessRunsOnTheNode(t *testing.T) {
	requireMesh(t)
	reg := registry(t)
	broker, _ := permission.New("auto")
	host := acphost.New(acphost.Config{Transport: reg.Transport(nodeA, "mock"), Permission: broker})
	t.Cleanup(host.Stop)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, _, err := host.OpenSession(ctx, "", acphost.SessionConfig{Workdir: "/home/pengxiang.lpx/steve-work"}); err != nil {
		t.Fatal(err)
	}
	out, err := sshOut(t, addrHost(addrA()), "pgrep -af mockagent | head -3")
	if err != nil {
		t.Skipf("cannot inspect %s over ssh: %v", nodeA, err)
	}
	if !strings.Contains(out, "mockagent") {
		t.Fatalf("no agent process found on %s; it may have run on the hub instead.\n%s", nodeA, out)
	}
	t.Logf("agent process on %s: %s", nodeA, strings.TrimSpace(out))
}

// A3: the reverse channel carries a request from the node's own loopback
// back to the hub, with the session's bearer token intact.
func TestA3ReverseMCPTunnel(t *testing.T) {
	requireMesh(t)
	reg := registry(t)

	var gotAuth, gotBody string
	received := make(chan struct{}, 1)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		select {
		case received <- struct{}{}:
		default:
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(hub.Close)
	reg.SetMCPDialer(func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", strings.TrimPrefix(hub.URL, "http://"))
	})

	endpoint, err := reg.MCPEndpoint(t.Context(), nodeA)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(endpoint, "http://127.0.0.1:") {
		t.Fatalf("endpoint = %q; a remote agent must be given a loopback address on its own machine", endpoint)
	}
	t.Logf("agents on %s call %s", nodeA, endpoint)

	// Call it from the node itself, the way an agent there would.
	curl := "curl -sS -X POST -H 'Authorization: Bearer sess-token-xyz' " +
		"-d '{\"jsonrpc\":\"2.0\",\"method\":\"tools/list\"}' " + endpoint
	out, err := sshOut(t, addrHost(addrA()), curl)
	if err != nil {
		t.Fatalf("call from the node failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, `"ok":true`) {
		t.Fatalf("response through the tunnel = %q", out)
	}
	select {
	case <-received:
	case <-time.After(10 * time.Second):
		t.Fatal("the hub never saw the request")
	}
	if gotAuth != "Bearer sess-token-xyz" {
		t.Errorf("per-session token did not survive the tunnel: %q", gotAuth)
	}
	if !strings.Contains(gotBody, "tools/list") {
		t.Errorf("body did not survive the tunnel: %q", gotBody)
	}
}

// A5: placement respects what a node actually advertises.
func TestA5CapabilityIsolation(t *testing.T) {
	requireMesh(t)
	reg := registry(t)
	adA, err := reg.Advert(t.Context(), nodeA)
	if err != nil {
		t.Fatal(err)
	}
	adB, err := reg.Advert(t.Context(), nodeB)
	if err != nil {
		t.Fatal(err)
	}
	if has(adB.Capabilities, "gpu") {
		t.Fatalf("%s should not claim gpu: %v", nodeB, adB.Capabilities)
	}
	if has(adA.Capabilities, "internal-net") {
		t.Fatalf("%s should not claim internal-net: %v", nodeA, adA.Capabilities)
	}
	t.Logf("gpu -> %s only; internal-net -> %s only", nodeA, nodeB)
}

func addrHost(addr string) string {
	host, _, ok := strings.Cut(addr, ":")
	if !ok {
		return addr
	}
	return host
}

// sshOut runs a command on a node. The suite already assumes operator access
// to these hosts — it started the nodes there.
func sshOut(t *testing.T, host, command string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=no", "-n", host, command)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// A4: a node going away must fail its sessions, not hang them — and the hub
// must reconnect on its own once the node is back.
func TestA4NodeDropAndReconnect(t *testing.T) {
	requireMesh(t)
	reg := registry(t)
	host := addrHost(addrB())

	broker, _ := permission.New("auto")
	acp := acphost.New(acphost.Config{Transport: reg.Transport(nodeB, "mock"), Permission: broker})
	t.Cleanup(acp.Stop)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	sid, generation, err := acp.OpenSession(ctx, "", acphost.SessionConfig{Workdir: "/home/pengxiang.lpx/steve-work"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := acp.Prompt(ctx, sid, generation, "before the drop", nil); err != nil {
		t.Fatal(err)
	}

	// Pull the node out from under the live session.
	if out, err := sshOut(t, host, "~/steve-bin/nodectl stop"); err != nil {
		t.Fatalf("stop node: %v\n%s", err, out)
	}
	t.Cleanup(func() { _, _ = sshOut(t, host, "~/steve-bin/nodectl start") })

	// The next prompt must fail promptly rather than block forever.
	failed := make(chan error, 1)
	go func() {
		_, _, err := acp.Prompt(ctx, sid, generation, "after the drop", nil)
		failed <- err
	}()
	select {
	case err := <-failed:
		if err == nil {
			t.Fatal("a prompt succeeded against a node that is gone")
		}
		t.Logf("session failed as expected: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("prompt hung after the node went away")
	}

	// The registry must report it down without being asked to re-probe.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		for _, s := range reg.Statuses() {
			if s.Name == nodeB && !s.Up {
				goto down
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("registry still reports %s as up after it stopped", nodeB)

down:
	t.Logf("%s reported down", nodeB)

	// Bring it back; a fresh session must connect without restarting the hub.
	if out, err := sshOut(t, host, "~/steve-bin/nodectl start"); err != nil {
		t.Fatalf("restart node: %v\n%s", err, out)
	}
	var advert error
	for range 20 {
		if _, advert = reg.Advert(t.Context(), nodeB); advert == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if advert != nil {
		t.Fatalf("hub never reconnected to %s: %v", nodeB, advert)
	}
	revived := acphost.New(acphost.Config{Transport: reg.Transport(nodeB, "mock"), Permission: broker})
	t.Cleanup(revived.Stop)
	sid2, gen2, err := revived.OpenSession(ctx, "", acphost.SessionConfig{Workdir: "/home/pengxiang.lpx/steve-work"})
	if err != nil {
		t.Fatalf("session after reconnect: %v", err)
	}
	out, _, err := revived.Prompt(ctx, sid2, gen2, "after the return", nil)
	if err != nil || out != "echo: after the return" {
		t.Fatalf("prompt after reconnect = %q, %v", out, err)
	}
	t.Logf("reconnected and answered: %q", out)
}
