// Package mesh is the three-host acceptance suite. It talks to real
// steve-node processes over a real network, so it is opt-in:
//
//	STEVE_MESH_E2E=1 go test ./e2e/mesh/ -v
//
// The machines come from e2e/fleetlab: a container per node by default,
// or hardware the operator names in the environment for the runs that
// need real agents. Either way the suite holds no addresses of its own.
//
// Scenario numbers refer to SCENARIOS.md in this directory.
package mesh

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/e2e/fleetlab"
	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/roster"
	"path/filepath"
)

const (
	nodeA = "node-a"
	nodeB = "node-b"
)

// machines is the fleet the whole suite runs against. Scenarios share it
// the way they shared two standing hosts before: bringing a fleet up per
// scenario would cost more than every scenario put together.
var (
	machines    *fleetlab.Lab
	machinesErr error
)

// The capabilities each node offers are what the placement scenarios sort
// by: one machine claims a GPU, the other an internal network and a
// production credential. None can be checked from inside a process, which
// is the point — placement has to take the machine at its word.
func TestMain(m *testing.M) {
	if os.Getenv("STEVE_MESH_E2E") != "" {
		machines, machinesErr = fleetlab.Open(
			fleetlab.Spec{Name: nodeA, Capabilities: []string{"gpu"}},
			fleetlab.Spec{Name: nodeB, Capabilities: []string{"internal-net", "prod-cred"}},
		)
	}
	code := m.Run()
	if machines != nil {
		machines.Close()
	}
	os.Exit(code)
}

// requireMesh gates a scenario on having a fleet. Missing Docker on a
// machine that was never going to run this is a skip; an operator who
// asked for the suite and got no fleet is told why it failed.
func requireMesh(t *testing.T) *fleetlab.Lab {
	t.Helper()
	if os.Getenv("STEVE_MESH_E2E") == "" {
		t.Skip("set STEVE_MESH_E2E=1 to run the three-host suite")
	}
	if machinesErr != nil {
		if errors.Is(machinesErr, fleetlab.ErrUnavailable) {
			t.Skipf("no machines to run on: %v", machinesErr)
		}
		t.Fatalf("the fleet did not come up: %v", machinesErr)
	}
	return machines
}

// work is the directory a node's projects and sessions are homed at.
func work(nodeName string) string { return machines.Work(nodeName) }

// shellPath keeps a lab-provided path literal in a node-side command.
func shellPath(path string) string { return "'" + strings.ReplaceAll(path, "'", "'\"'\"'") + "'" }

func registry(t *testing.T) *node.Registry {
	t.Helper()
	reg := node.NewRegistry("hub-e2e", map[string]node.Config{
		nodeA: {Addr: machines.Addr(nodeA), Token: machines.Token(nodeA), DialTimeout: 10 * time.Second},
		nodeB: {Addr: machines.Addr(nodeB), Token: machines.Token(nodeB), DialTimeout: 10 * time.Second},
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
	done := make(chan error, 1)
	go func() { done <- broken.Serve(ctx) }()
	t.Cleanup(func() {
		// The registry closes first. Join Serve so its handlers and
		// background writers finish before TempDir removes the state.
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("fixture node serve: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("fixture node did not stop serving")
		}
	})
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
			sid, generation, err := host.OpenSession(ctx, "", acphost.SessionConfig{Workdir: work(nodeName)})
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
	if _, _, err := host.OpenSession(ctx, "", acphost.SessionConfig{Workdir: work(nodeA)}); err != nil {
		t.Fatal(err)
	}
	out, err := onNode(t, nodeA, "pgrep -af mockagent | head -3")
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
	out, err := onNode(t, nodeA, curl)
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

// onNode runs a command on the machine a node runs on. Proving where
// something happened — the marker file, the process, the log line — means
// reading it from that machine and nowhere else.
func onNode(t *testing.T, nodeName, command string) (string, error) {
	t.Helper()
	return machines.Exec(nodeName, command)
}

// A4: a node going away must fail its sessions, not hang them — and the hub
// must reconnect on its own once the node is back.
func TestA4NodeDropAndReconnect(t *testing.T) {
	requireMesh(t)
	lab := requireMesh(t)
	reg := registry(t)

	broker, _ := permission.New("auto")
	acp := acphost.New(acphost.Config{Transport: reg.Transport(nodeB, "mock"), Permission: broker})
	t.Cleanup(acp.Stop)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	sid, generation, err := acp.OpenSession(ctx, "", acphost.SessionConfig{Workdir: work(nodeB)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := acp.Prompt(ctx, sid, generation, "before the drop", nil); err != nil {
		t.Fatal(err)
	}

	// Pull the node out from under the live session.
	if err := lab.StopNode(nodeB); err != nil {
		t.Fatalf("stop node: %v", err)
	}
	t.Cleanup(func() { _ = lab.StartNode(nodeB) })

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
	if err := lab.StartNode(nodeB); err != nil {
		t.Fatalf("restart node: %v", err)
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
	sid2, gen2, err := revived.OpenSession(ctx, "", acphost.SessionConfig{Workdir: work(nodeB)})
	if err != nil {
		t.Fatalf("session after reconnect: %v", err)
	}
	out, _, err := revived.Prompt(ctx, sid2, gen2, "after the return", nil)
	if err != nil || out != "echo: after the return" {
		t.Fatalf("prompt after reconnect = %q, %v", out, err)
	}
	t.Logf("reconnected and answered: %q", out)
}

// A1c: placement reads each machine's manifest. A step that needs a tool
// lands on the machine that observed it; one that needs a tool nobody has
// is refused with the selector and the reason, per machine.
func TestA1PlacementFollowsTheManifest(t *testing.T) {
	requireMesh(t)
	fixture := node.NewServer(node.ServerConfig{
		Name: "lab", Token: "lab-token", StateDir: t.TempDir(), Listen: "127.0.0.1:0",
		Harnesses: map[string]node.HarnessSpec{"mock": {Command: mockAgentPath(t)}},
		Tools:     []string{"sh", "no-such-tool"},
		Declares:  []string{"network:lab"},
	})
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go func() { _ = fixture.Serve(ctx) }()
	for range 100 {
		if addr := fixture.Addr(); addr != "" && !strings.HasSuffix(addr, ":0") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	reg := node.NewRegistry("hub-e2e", map[string]node.Config{"lab": {Addr: fixture.Addr(), Token: "lab-token"}})
	t.Cleanup(reg.Close)
	catalog, err := agent.NewCatalog(map[string]agent.Config{
		"local":  {Harness: "mock", Default: true},
		"labber": {Harness: "mock", Node: "lab"},
	})
	if err != nil {
		t.Fatal(err)
	}
	fleet := roster.New(catalog)
	fleet.SetNodes(reg)
	fleet.SetHubAdvert(func() nodewire.Advert {
		return nodewire.Advert{Harnesses: []nodewire.Harness{{ID: "mock", Command: "mockagent"}}, Snapshot: node.Snapshot("hub-e2e", 1, 1, node.Observe{Harnesses: map[string]node.HarnessSpec{"mock": {Command: mockAgentPath(t)}}})}
	})
	names := func(cs []roster.Candidate) []string {
		var out []string
		for _, c := range cs {
			out = append(out, c.Agent.ID)
		}
		return out
	}
	// Only the fixture was asked to look for sh; the hub covered tools too
	// (it observed none), so it is absent there rather than unknown.
	if got := names(fleet.Candidates(t.Context(), []string{"tool:sh"}, nil)); len(got) != 1 || got[0] != "labber" {
		t.Fatalf("tool:sh → %v", got)
	}
	if got := names(fleet.Candidates(t.Context(), []string{"network:lab"}, nil)); len(got) != 0 {
		t.Fatalf("a declared network placed work: %v (network is observed only until probes exist)", got)
	}
	if got := names(fleet.Candidates(t.Context(), []string{"tool:no-such-tool"}, nil)); len(got) != 0 {
		t.Fatalf("a missing tool placed work: %v", got)
	}
	why := fleet.Explain(t.Context(), []string{"tool:no-such-tool"})
	if !strings.Contains(why, "labber: lacks tool:no-such-tool (UNAVAILABLE") || !strings.Contains(why, "local: lacks tool:no-such-tool (ABSENT") {
		t.Fatalf("explain = %s", why)
	}
}

// mockAgentPath builds the ACP test agent for an in-process node.
func mockAgentPath(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "mockagent")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent")
	cmd.Dir = "../.."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build mockagent: %v\n%s", err, out)
	}
	return bin
}
