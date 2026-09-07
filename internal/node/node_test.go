package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

	"github.com/gopact-ai/steve/internal/ability"
	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/permission"
	steveruntime "github.com/gopact-ai/steve/internal/runtime"
	"github.com/gopact-ai/steve/internal/skills"
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
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("node serve: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("node did not stop serving")
		}
		// Registry.Close runs first. Its disconnected handler can still be
		// writing hub.json after Serve returns; TempDir must not remove the
		// state directory until release has finished that write.
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			server.hubMu.Lock()
			live := server.hubLive
			server.hubMu.Unlock()
			if live == 0 {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Error("node did not finish releasing its hub")
	})
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

// A node reports a snapshot: harnesses and tools it checked on its PATH,
// MCP servers it can start (per harness scope), the hardware it sees, and
// the declarations it was given — each with its evidence, and coverage
// saying which kinds were fully checked. Nothing secret rides along.
func TestNodeReportsASnapshot(t *testing.T) {
	bin := buildMockAgent(t)
	server := startNode(t, ServerConfig{
		Name: "host-4", Token: "tok", StateDir: t.TempDir(),
		Harnesses:    map[string]HarnessSpec{"codex": {Command: bin}},
		Tools:        []string{"sh", "definitely-not-a-tool"},
		MCPServers:   map[string]MCPSpec{"local": {Type: "stdio", Command: "sh", Env: map[string]string{"TOKEN": "SECRET"}}, "gone": {Type: "stdio", Command: "no-such-mcp"}},
		Declares:     []string{"network:internal", "credential:prod"},
		Capabilities: []string{"build"},
	})
	registry := NewRegistry("hub-1", map[string]Config{"host-4": {Addr: server.Addr(), Token: "tok"}})
	t.Cleanup(registry.Close)
	advert, err := registry.Advert(t.Context(), "host-4")
	if err != nil {
		t.Fatal(err)
	}
	snap := advert.Snapshot
	if snap == nil || snap.Schema != ability.Schema || snap.Digest == "" || snap.ReceivedAt.IsZero() {
		t.Fatalf("snapshot = %+v", snap)
	}
	if !nodewire.HasFeature(advert.Features, nodewire.FeatureManifest) {
		t.Fatalf("features = %v", advert.Features)
	}
	got := map[string]ability.Availability{}
	for _, c := range snap.Offers {
		got[c.Key()] = c.Availability
		if strings.Contains(c.Detail, "SECRET") {
			t.Fatalf("env leaked into the snapshot: %+v", c)
		}
	}
	for key, want := range map[string]ability.Availability{
		"harness:codex": ability.Available, "tool:sh": ability.Available, "tool:definitely-not-a-tool": ability.Unavailable,
		"mcp:local@codex": ability.Available, "mcp:gone@codex": ability.Unavailable,
		"network:internal": ability.Unknown, "credential:prod": ability.Unknown, "tag:build": ability.Available,
		"hardware:cpu": ability.Available,
	} {
		if got[key] != want {
			t.Errorf("%s = %q, want %q", key, got[key], want)
		}
	}
	if snap.Coverage[ability.Tool] != ability.Complete || snap.Coverage[ability.Skill] != ability.Complete {
		t.Fatalf("coverage = %v", snap.Coverage)
	}
}

// A node serves one hub at a time: a second hub with the right token is
// refused while the first is connected.
func TestNodeRefusesASecondHub(t *testing.T) {
	server := startNode(t, ServerConfig{Name: "host-5", Token: "tok", StateDir: t.TempDir(), Harnesses: map[string]HarnessSpec{"codex": {Command: buildMockAgent(t)}}})
	first := NewRegistry("hub-1", map[string]Config{"host-5": {Addr: server.Addr(), Token: "tok"}})
	t.Cleanup(first.Close)
	if _, err := first.Advert(t.Context(), "host-5"); err != nil {
		t.Fatal(err)
	}
	second := NewRegistry("hub-2", map[string]Config{"host-5": {Addr: server.Addr(), Token: "tok"}})
	t.Cleanup(second.Close)
	if _, err := second.Advert(t.Context(), "host-5"); err == nil {
		t.Fatal("a second hub was served alongside the first")
	}
}

// Admission is the node's word on what it has now, not what it said at the
// handshake: each request observes afresh, so the reply carries a newer
// sequence than the advert, and a tool that is not there is refused with
// the atom that failed rather than an error.
func TestNodeAdmitsOnAFreshObservation(t *testing.T) {
	bin := buildMockAgent(t)
	server := startNode(t, ServerConfig{
		Name: "host-6", Token: "tok", StateDir: t.TempDir(),
		Harnesses: map[string]HarnessSpec{"codex": {Command: bin}},
		Tools:     []string{"sh", "definitely-not-a-tool"},
	})
	registry := NewRegistry("hub-1", map[string]Config{"host-6": {Addr: server.Addr(), Token: "tok"}})
	t.Cleanup(registry.Close)
	advert, err := registry.Advert(t.Context(), "host-6")
	if err != nil {
		t.Fatal(err)
	}
	present, _ := ability.Compile([]string{"tool:sh", "harness:codex"})
	adm, err := registry.Admit(t.Context(), "host-6", nodewire.AdmitRequest{Attempt: "a1", Harness: "codex", Requirement: present})
	if err != nil {
		t.Fatal(err)
	}
	if !adm.OK() || adm.Source != ability.SourceNode || adm.Node != "host-6" || adm.Code != ability.CodeAdmitted {
		t.Fatalf("admission = %+v", adm)
	}
	if adm.Generation != advert.Snapshot.Generation || adm.Sequence <= advert.Snapshot.Sequence {
		t.Fatalf("admission judged on %d/%d, advert was %d/%d: not a fresh observation", adm.Generation, adm.Sequence, advert.Snapshot.Generation, advert.Snapshot.Sequence)
	}
	absent, _ := ability.Compile([]string{"tool:definitely-not-a-tool"})
	adm, err = registry.Admit(t.Context(), "host-6", nodewire.AdmitRequest{Attempt: "a2", Harness: "codex", Requirement: absent})
	if err != nil {
		t.Fatal(err)
	}
	if !adm.Refused() || adm.Code != ability.CodeUnavailable {
		t.Fatalf("a missing tool should be refused as UNAVAILABLE, got %+v", adm)
	}
}

// The hub keeps a connected node's snapshot fresh on its own: evidence has
// a TTL, and a hub that only read the handshake would a quarter of an hour
// later be unable to place anything that needs a tool.
func TestHubRefreshesSnapshotsOnItsOwn(t *testing.T) {
	old := RefreshEvery
	RefreshEvery = 150 * time.Millisecond
	t.Cleanup(func() { RefreshEvery = old })
	bin := buildMockAgent(t)
	server := startNode(t, ServerConfig{
		Name: "host-7", Token: "tok", StateDir: t.TempDir(),
		Harnesses: map[string]HarnessSpec{"codex": {Command: bin}}, Tools: []string{"sh"},
	})
	registry := NewRegistry("hub-1", map[string]Config{"host-7": {Addr: server.Addr(), Token: "tok"}})
	t.Cleanup(registry.Close)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	registry.Start(ctx)
	first, err := registry.Advert(ctx, "host-7")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		adv, _ := registry.Advert(ctx, "host-7")
		if adv.Snapshot != nil && adv.Snapshot.Sequence > first.Snapshot.Sequence+1 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("snapshot sequence stayed at %d; the hub never refreshed it", first.Snapshot.Sequence)
}

// The hub ships its enabled skills to a node as a content-addressed
// bundle; the node checks the hash, materializes every skill into every
// isolated harness home, and reports them by content in its snapshot —
// so "skill:deploy" places only where deploy is really on disk. The
// harness homes themselves are the node's own, not the user's.
func TestSkillsArePushedAndMaterialized(t *testing.T) {
	bin := buildMockAgent(t)
	state := t.TempDir()
	server := startNode(t, ServerConfig{
		Name: "host-8", Token: "tok", StateDir: state,
		Harnesses: map[string]HarnessSpec{"codex": {Command: bin}, "claude-code": {Command: bin}},
	})
	registry := NewRegistry("hub-1", map[string]Config{"host-8": {Addr: server.Addr(), Token: "tok"}})
	t.Cleanup(registry.Close)
	before, err := registry.Advert(t.Context(), "host-8")
	if err != nil {
		t.Fatal(err)
	}
	if before.Skills != "" || !nodewire.HasFeature(before.Features, nodewire.FeatureSkills) {
		t.Fatalf("fresh node advert = skills %q features %v", before.Skills, before.Features)
	}
	if _, err := os.Stat(filepath.Join(state, "runtimes", "codex")); err != nil {
		t.Fatalf("no isolated codex home: %v", err)
	}

	src := t.TempDir()
	_ = os.MkdirAll(filepath.Join(src, "deploy"), 0o755)
	_ = os.WriteFile(filepath.Join(src, "deploy", "SKILL.md"), []byte("# deploy\n"), 0o644)
	bundle, err := skills.Pack([]skills.Ref{{Name: "deploy", Path: filepath.Join(src, "deploy")}})
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.PushSkills(t.Context(), "host-8", bundle); err != nil {
		t.Fatal(err)
	}
	for _, dest := range steveruntime.SelectedSkillDests(state, []string{"codex", "claude-code"}) {
		if _, err := os.ReadFile(filepath.Join(dest, "deploy", "SKILL.md")); err != nil {
			t.Errorf("skill not materialized in %s: %v", dest, err)
		}
	}
	for _, unselected := range []string{"grok", "kimi"} {
		if _, err := os.Stat(filepath.Join(state, "runtimes", unselected)); !os.IsNotExist(err) {
			t.Errorf("unregistered tool %s runtime was created: %v", unselected, err)
		}
	}
	after, err := registry.Refresh(t.Context(), "host-8")
	if err != nil {
		t.Fatal(err)
	}
	if after.Skills != bundle.Hash {
		t.Fatalf("advert skills = %q, want %s", after.Skills, bundle.Hash)
	}
	var found []string
	for _, c := range after.Snapshot.Offers {
		if c.Kind == ability.Skill {
			found = append(found, c.Key())
			if c.Availability != ability.Available || c.Version == nil || c.Version.Value != bundle.Skills[0].Hash {
				t.Errorf("skill offer %+v", c)
			}
		}
	}
	if len(found) != 2 || after.Snapshot.Coverage[ability.Skill] != ability.Complete {
		t.Fatalf("skill offers = %v, coverage %s", found, after.Snapshot.Coverage[ability.Skill])
	}
	req, _ := ability.Compile([]string{"skill:deploy"})
	if m := ability.Match(req, after.Snapshot, "codex", time.Now()); !m.OK() {
		t.Fatalf("skill:deploy should match on codex: %s", m.Unmet())
	}
	if m := ability.Match(req, before.Snapshot, "codex", time.Now()); m.Verdict != ability.False {
		t.Fatalf("before the push, skill:deploy should be absent, got %v", m.Verdict)
	}
	// Pushing the same bundle again is a no-op; a tampered blob is refused.
	if err := registry.PushSkills(t.Context(), "host-8", bundle); err != nil {
		t.Fatal(err)
	}
	bad := bundle
	bad.Hash = "0000000000000000000000000000000000000000000000000000000000000000"
	if err := registry.PushSkills(t.Context(), "host-8", bad); err == nil {
		t.Fatal("a bundle whose bytes do not match its hash was applied")
	}
}

// An MCP server's command and credentials stay on the node. Admission
// with uses binds the server for the attempt and hands back a launcher
// that names only this binary, the broker socket and a binding id; the
// launcher reaches the real server, started by the node with its env, and
// nothing the hub receives carries the secret. A server the node does not
// have refuses the admission; a binding the broker never minted goes
// nowhere.
func TestMCPBindingKeepsSecretsOnTheNode(t *testing.T) {
	bin := buildMockAgent(t)
	state := t.TempDir()
	// A real HTTP MCP server would live here; this one shows what reached
	// it, so the proxy's header injection can be seen from outside.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "path=%s auth=%s", r.URL.Path, r.Header.Get("Authorization"))
	}))
	t.Cleanup(upstream.Close)
	server := startNode(t, ServerConfig{
		Name: "host-9", Token: "tok", StateDir: state,
		Harnesses: map[string]HarnessSpec{"codex": {Command: bin}},
		MCPServers: map[string]MCPSpec{
			"echo": {Type: "stdio", Command: "sh", Args: []string{"-c", `read line; echo "got $line via $TOKEN"`}, Env: map[string]string{"TOKEN": "SECRET-42"}},
			"web":  {Type: "http", URL: upstream.URL + "/mcp", Headers: map[string]string{"Authorization": "Bearer WEB-SECRET"}},
		},
	})
	registry := NewRegistry("hub-1", map[string]Config{"host-9": {Addr: server.Addr(), Token: "tok"}})
	t.Cleanup(registry.Close)
	advert, err := registry.Advert(t.Context(), "host-9")
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := json.Marshal(advert); strings.Contains(string(raw), "SECRET-42") {
		t.Fatal("the advert carries the MCP secret")
	}
	if !nodewire.HasFeature(advert.Features, nodewire.FeatureMCP) {
		t.Fatalf("features = %v", advert.Features)
	}
	var echo, web ability.Capability
	for _, c := range advert.Snapshot.Offers {
		if c.Kind == ability.MCP && c.Scope == "codex" {
			switch c.ID {
			case "echo":
				echo = c
			case "web":
				web = c
			}
		}
	}
	if echo.Availability != ability.Available || web.Availability != ability.Available {
		t.Fatalf("echo = %s, web = %s", echo.Availability, web.Availability)
	}
	if raw, _ := json.Marshal(advert); strings.Contains(string(raw), "WEB-SECRET") {
		t.Fatal("the advert carries the HTTP header secret")
	}

	req, _ := ability.Compile(nil)
	adm, err := registry.Admit(t.Context(), "host-9", nodewire.AdmitRequest{Attempt: "a-mcp", Harness: "codex", Requirement: req, Uses: []string{"echo"}})
	if err != nil {
		t.Fatal(err)
	}
	if !adm.OK() || len(adm.Bound) != 1 || adm.Bound[0] != "echo" {
		t.Fatalf("admission = %+v", adm)
	}
	bindings := registry.Bindings(t.Context(), "host-9", "a-mcp")
	if len(bindings) != 1 || bindings[0].Name != "echo" || len(bindings[0].Args) != 4 || bindings[0].Args[0] != LaunchVerb {
		t.Fatalf("bindings = %+v", bindings)
	}
	if raw, _ := json.Marshal(bindings); strings.Contains(string(raw), "SECRET-42") || strings.Contains(string(raw), "TOKEN") {
		t.Fatal("the binding carries the secret")
	}
	if again := registry.Bindings(t.Context(), "host-9", "a-mcp"); again != nil {
		t.Fatal("bindings were handed out twice")
	}
	socket, id := bindings[0].Args[2], bindings[0].Args[3]
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := LaunchBinding(ctx, socket, id, strings.NewReader("hello\n"), &out); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != "got hello via SECRET-42" {
		t.Fatalf("through the broker: %q", got)
	}
	out.Reset()
	if err := LaunchBinding(ctx, socket, "not-a-binding", strings.NewReader("hello\n"), &out); err != nil || out.Len() != 0 {
		t.Fatalf("an unknown binding produced %q, %v", out.String(), err)
	}

	adm, err = registry.Admit(t.Context(), "host-9", nodewire.AdmitRequest{Attempt: "a-missing", Harness: "codex", Requirement: req, Uses: []string{"echo", "github"}})
	if err != nil {
		t.Fatal(err)
	}
	if !adm.Refused() || adm.Code != ability.CodeAbsent || len(adm.Bound) != 0 {
		t.Fatalf("a server the node lacks should refuse: %+v", adm)
	}
	if registry.Bindings(t.Context(), "host-9", "a-missing") != nil {
		t.Fatal("a refused admission handed out bindings")
	}
	// An HTTP server is reached through the node's loopback proxy, which
	// adds the configured headers; the agent's URL names only a binding.
	adm, err = registry.Admit(t.Context(), "host-9", nodewire.AdmitRequest{Attempt: "a-http", Harness: "codex", Requirement: req, Uses: []string{"web"}})
	if err != nil || !adm.OK() {
		t.Fatalf("http admission = %+v, %v", adm, err)
	}
	web1 := registry.Bindings(t.Context(), "host-9", "a-http")
	if len(web1) != 1 || web1[0].Transport != "http" || !strings.HasPrefix(web1[0].URL, "http://127.0.0.1:") || strings.Contains(web1[0].URL, "WEB-SECRET") {
		t.Fatalf("http binding = %+v", web1)
	}
	res, err := http.Get(web1[0].URL + "/tools")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if string(body) != "path=/mcp/tools auth=Bearer WEB-SECRET" {
		t.Fatalf("through the proxy: %q", body)
	}
	// Release ends the attempt's bindings: the launcher and the URL both die.
	if err := registry.Release(t.Context(), "host-9", "a-http"); err != nil {
		t.Fatal(err)
	}
	if res, err := http.Get(web1[0].URL + "/tools"); err != nil || res.StatusCode != http.StatusForbidden {
		t.Fatalf("after release the proxy answered %v, %v", res, err)
	}
	if err := registry.Release(t.Context(), "host-9", "a-mcp"); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := LaunchBinding(ctx, socket, id, strings.NewReader("hello\n"), &out); err != nil || out.Len() != 0 {
		t.Fatalf("a released binding still launched: %q, %v", out.String(), err)
	}
}

// A node keeps its owner through both clean disconnects and silence. The
// same hub may reconnect; another must wait for explicit offline adoption.
func TestNodeRemembersItsHub(t *testing.T) {
	bin := buildMockAgent(t)
	state := t.TempDir()
	server := startNode(t, ServerConfig{Name: "host-10", Token: "tok", StateDir: state, Harnesses: map[string]HarnessSpec{"codex": {Command: bin}}})
	first := NewRegistry("hub-1", map[string]Config{"host-10": {Addr: server.Addr(), Token: "tok"}})
	t.Cleanup(first.Close)
	if _, err := first.Advert(t.Context(), "host-10"); err != nil {
		t.Fatal(err)
	}
	first.Close()
	waitDisconnected := func() {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			server.hubMu.Lock()
			live := server.hubLive
			server.hubMu.Unlock()
			if live == 0 {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal("hub did not disconnect")
	}
	waitDisconnected()
	if owner := server.owner(); owner.Hub != "hub-1" || !owner.Released {
		t.Fatalf("clean disconnect did not retain owner and stop evidence: %+v", owner)
	}
	second := NewRegistry("hub-2", map[string]Config{"host-10": {Addr: server.Addr(), Token: "tok"}})
	t.Cleanup(second.Close)
	if _, err := second.Advert(t.Context(), "host-10"); !errors.Is(err, nodewire.ErrRefused) {
		t.Fatalf("clean disconnect allowed another hub to take ownership: %v", err)
	}
	reconnected := NewRegistry("hub-1", map[string]Config{"host-10": {Addr: server.Addr(), Token: "tok"}})
	t.Cleanup(reconnected.Close)
	if _, err := reconnected.Advert(t.Context(), "host-10"); err != nil {
		t.Fatalf("same owner could not reconnect: %v", err)
	}
	reconnected.Close()
	waitDisconnected()
	// Even an arbitrarily old record does not authorize a different hub.
	if err := server.writeOwner(hubOwner{Hub: "hub-1", LastSeen: time.Now().Add(-365 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Advert(t.Context(), "host-10"); !errors.Is(err, nodewire.ErrRefused) {
		t.Fatalf("timeout granted ownership to another hub: %v", err)
	}
	if err := Adopt(state, "hub-2"); err == nil {
		t.Fatal("adoption changed a running node instance")
	}
	if owner := server.owner(); owner.Hub != "hub-1" {
		t.Fatalf("refused handshake changed owner: %+v", owner)
	}
}

// A token can vouch for a hub's name: a hub presenting hub-1's token
// while calling itself hub-2 is refused, so the name a node remembers is
// one its configuration tied to a secret, not whatever the caller said.
func TestHubTokenVouchesForTheName(t *testing.T) {
	bin := buildMockAgent(t)
	server := startNode(t, ServerConfig{Name: "host-11", StateDir: t.TempDir(), Hubs: map[string]string{"hub-1": "secret-1"},
		Harnesses: map[string]HarnessSpec{"codex": {Command: bin}}})
	wrong := NewRegistry("hub-2", map[string]Config{"host-11": {Addr: server.Addr(), Token: "secret-1"}})
	t.Cleanup(wrong.Close)
	if _, err := wrong.Advert(t.Context(), "host-11"); err == nil || !errors.Is(err, nodewire.ErrRefused) {
		t.Fatalf("hub-2 used hub-1's token: %v", err)
	}
	right := NewRegistry("hub-1", map[string]Config{"host-11": {Addr: server.Addr(), Token: "secret-1"}})
	t.Cleanup(right.Close)
	adv, err := right.Advert(t.Context(), "host-11")
	if err != nil {
		t.Fatal(err)
	}
	if adv.Health == nil || adv.Health.DiskTotal == 0 {
		t.Fatalf("advert carries no health: %+v", adv.Health)
	}
	stranger := NewRegistry("hub-1", map[string]Config{"host-11": {Addr: server.Addr(), Token: "nope"}})
	t.Cleanup(stranger.Close)
	if _, err := stranger.Advert(t.Context(), "host-11"); err == nil || !errors.Is(err, nodewire.ErrBadToken) {
		t.Fatalf("an unknown token got in: %v", err)
	}
}

// The broker can be another process: the node then has no MCP servers of
// its own — nor their secrets — and reaches the broker over its socket
// with a control token. Listing, binding, launching and releasing all
// work the same; a caller without the token gets nothing.
func TestExternalBrokerHoldsTheSecrets(t *testing.T) {
	bin := buildMockAgent(t)
	dir := t.TempDir()
	socket := filepath.Join(dir, "mcp.sock")
	broker := NewBroker(BrokerConfig{Socket: socket, Token: "ctl-secret", PortFile: filepath.Join(dir, "proxy.port"),
		MCPServers: map[string]MCPSpec{"echo": {Type: "stdio", Command: "sh", Args: []string{"-c", `read line; echo "ext $line $TOKEN"`}, Env: map[string]string{"TOKEN": "S3"}}}})
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go func() { _ = broker.Serve(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(socket); err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	server := startNode(t, ServerConfig{Name: "host-12", Token: "tok", StateDir: t.TempDir(),
		Harnesses: map[string]HarnessSpec{"codex": {Command: bin}}, MCPBroker: &BrokerRef{Socket: socket, Token: "ctl-secret"}})
	registry := NewRegistry("hub-1", map[string]Config{"host-12": {Addr: server.Addr(), Token: "tok"}})
	t.Cleanup(registry.Close)
	advert, err := registry.Advert(t.Context(), "host-12")
	if err != nil {
		t.Fatal(err)
	}
	var listed bool
	for _, c := range advert.Snapshot.Offers {
		if c.Kind == ability.MCP && c.ID == "echo" && c.Scope == "codex" && c.Availability == ability.Available {
			listed = true
		}
	}
	if !listed || advert.Snapshot.Coverage[ability.MCP] != ability.Complete {
		t.Fatalf("the broker's servers are not in the snapshot: %v", advert.Snapshot.Coverage[ability.MCP])
	}
	req, _ := ability.Compile(nil)
	adm, err := registry.Admit(t.Context(), "host-12", nodewire.AdmitRequest{Attempt: "x-1", Harness: "codex", Requirement: req, Uses: []string{"echo"}})
	if err != nil || !adm.OK() {
		t.Fatalf("admission = %+v, %v", adm, err)
	}
	b := registry.Bindings(t.Context(), "host-12", "x-1")
	if len(b) != 1 || b[0].Args[2] != socket {
		t.Fatalf("bindings = %+v", b)
	}
	var out bytes.Buffer
	if err := LaunchBinding(ctx, socket, b[0].Args[3], strings.NewReader("hi\n"), &out); err != nil || strings.TrimSpace(out.String()) != "ext hi S3" {
		t.Fatalf("through the external broker: %q, %v", out.String(), err)
	}
	// The control commands need the token; a stranger on the socket is refused.
	if _, err := (remoteBroker{socket: socket, token: "wrong"}).List(ctx); err == nil {
		t.Fatal("LIST without the token succeeded")
	}
	if err := registry.Release(t.Context(), "host-12", "x-1"); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := LaunchBinding(ctx, socket, b[0].Args[3], strings.NewReader("hi\n"), &out); err != nil || out.Len() != 0 {
		t.Fatalf("a released binding still launched: %q", out.String())
	}
	adm, _ = registry.Admit(t.Context(), "host-12", nodewire.AdmitRequest{Attempt: "x-2", Harness: "codex", Requirement: req, Uses: []string{"nope"}})
	if !adm.Refused() || adm.Code != ability.CodeAbsent {
		t.Fatalf("a server the broker lacks: %+v", adm)
	}
}

// A machine is configured from the hub: the settings it sent come back as
// applied, its own file is rewritten so a restart keeps them, and the
// next snapshot shows the new tool, declaration and AI tool. A bad
// setting is refused whole and changes nothing.
func TestHubConfiguresANode(t *testing.T) {
	bin := buildMockAgent(t)
	state := t.TempDir()
	source := filepath.Join(state, "node.json")
	cfg := ServerConfig{Source: source, Name: "host-13", Token: "tok", StateDir: state, Listen: "127.0.0.1:0",
		Harnesses: map[string]HarnessSpec{"codex": {Adapter: "codex-acp", Command: bin, Slots: 3, Env: []string{"SECRET=keep"}}}, Tools: []string{"sh"}, MCPServers: map[string]MCPSpec{"private": {Type: "http", URL: "http://127.0.0.1:1", Env: map[string]string{"SECRET": "keep"}, Headers: map[string]string{"Authorization": "keep"}}}}
	raw, _ := json.Marshal(cfg)
	_ = os.WriteFile(source, raw, 0o600)
	server := startNode(t, cfg)
	registry := NewRegistry("hub-1", map[string]Config{"host-13": {Addr: server.Addr(), Token: "tok"}})
	t.Cleanup(registry.Close)
	got, err := registry.Settings(t.Context(), "host-13")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Harnesses) != 1 || got.Harnesses["codex"].Command != bin || len(got.Tools) != 1 {
		t.Fatalf("settings = %+v", got)
	}
	got.Tools = append(got.Tools, "git", "no-such-tool-xyz")
	got.Declares = []string{"network:lab"}
	got.Capabilities = []string{"gpu"}
	got.Harnesses["mock2"] = nodewire.HarnessSetting{Command: bin, Args: []string{"--flag"}}
	applied, err := registry.Configure(t.Context(), "host-13", got)
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision == "" || applied.Revision == got.Revision {
		t.Fatal("wire settings omitted/failed to advance revision")
	}
	if _, err := registry.Configure(t.Context(), "host-13", got); !errors.Is(err, nodewire.ErrSettingsRevisionConflict) {
		t.Fatalf("wire did not preserve typed stale revision: %v", err)
	}
	missing := applied
	missing.Revision = ""
	if _, err := registry.Configure(t.Context(), "host-13", missing); !errors.Is(err, nodewire.ErrSettingsRevisionConflict) {
		t.Fatalf("wire accepted missing revision: %v", err)
	}
	if len(applied.Harnesses) != 2 || len(applied.Tools) != 3 || applied.Declares[0] != "network:lab" {
		t.Fatalf("applied = %+v", applied)
	}
	pin := applied.Harnesses["codex"]
	if pin.Adapter == nil || *pin.Adapter != "codex-acp" || pin.Slots == nil || *pin.Slots != 3 || pin.Env[0] != "SECRET=keep" || applied.MCPServers["private"].Headers["Authorization"] != "keep" {
		t.Fatal("wire roundtrip lost hidden fields")
	}
	// The file the node started from now says the same.
	var onDisk ServerConfig
	saved, _ := os.ReadFile(source)
	if err := json.Unmarshal(saved, &onDisk); err != nil {
		t.Fatal(err)
	}
	if onDisk.Token != "tok" || len(onDisk.Harnesses) != 2 || onDisk.Harnesses["mock2"].Args[0] != "--flag" || len(onDisk.Tools) != 3 || onDisk.Capabilities[0] != "gpu" {
		t.Fatalf("node.json after configure = %+v", onDisk)
	}
	// The snapshot the hub holds reflects it.
	adv, _ := registry.Advert(t.Context(), "host-13")
	keys := map[string]ability.Availability{}
	for _, c := range adv.Snapshot.Offers {
		keys[c.Key()] = c.Availability
	}
	if keys["harness:mock2"] != ability.Available || keys["tool:git"] != ability.Available || keys["tool:no-such-tool-xyz"] != ability.Unavailable || keys["tag:gpu"] != ability.Available {
		t.Fatalf("offers after configure = %v", keys)
	}
	// An invalid command is refused and leaves the current settings in force.
	bad := applied
	bad.Harnesses = map[string]nodewire.HarnessSetting{"invalid": {Command: ""}}
	if _, err := registry.Configure(t.Context(), "host-13", bad); err == nil {
		t.Fatal("a configured tool without a command was accepted")
	}
	again, _ := registry.Settings(t.Context(), "host-13")
	if len(again.Harnesses) != 2 {
		t.Fatalf("a refused setting changed the node: %+v", again)
	}
	clear := nodewire.CloneSettings(again)
	zero := 0
	pin = clear.Harnesses["codex"]
	pin.Slots = &zero
	pin.Env = []string{}
	clear.Harnesses["codex"] = pin
	secret := clear.MCPServers["private"]
	secret.Env = map[string]string{}
	secret.Headers = map[string]string{}
	clear.MCPServers["private"] = secret
	again, err = registry.Configure(t.Context(), "host-13", clear)
	if err != nil {
		t.Fatal(err)
	}
	if *again.Harnesses["codex"].Slots != 0 || len(again.Harnesses["codex"].Env) != 0 || len(again.MCPServers["private"].Env) != 0 || len(again.MCPServers["private"].Headers) != 0 {
		t.Fatal("explicit clear was lost in actual wire encoding")
	}
	connection, err := registry.connect(t.Context(), "host-13")
	if err != nil {
		t.Fatal(err)
	}
	oldAdvert := connection.getAdvert()
	oldAdvert.Features = append([]string(nil), oldAdvert.Features...)
	for i, feature := range oldAdvert.Features {
		if feature == nodewire.FeatureConfigRevision {
			oldAdvert.Features = append(oldAdvert.Features[:i:i], oldAdvert.Features[i+1:]...)
			break
		}
	}
	connection.setAdvert(oldAdvert)
	unprotected := again
	unprotected.Tools = append(append([]string(nil), again.Tools...), "must-not-apply")
	if _, err := registry.Configure(t.Context(), "host-13", unprotected); !errors.Is(err, nodewire.ErrSettingsRevisionUnsupported) {
		t.Fatalf("old node accepted unprotected set: %v", err)
	}
	unchanged, err := registry.Settings(t.Context(), "host-13")
	if err != nil || unchanged.Revision != again.Revision {
		t.Fatalf("unsupported revision set reached node: %+v %v", unchanged, err)
	}
}

// The hub dials its machines before its messaging server exists; the
// dialer wired afterwards must reach the connections already up, not
// only the next redial.
func TestReverseMCPTunnelReachesConnectionsAlreadyUp(t *testing.T) {
	hubServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"late":true}`))
	}))
	t.Cleanup(hubServer.Close)
	hubAddr := strings.TrimPrefix(hubServer.URL, "http://")

	server := startNode(t, ServerConfig{
		Name: "host-4", Token: "tok", StateDir: t.TempDir(),
		Harnesses: map[string]HarnessSpec{"codex": {Command: "echo"}},
	})
	registry := NewRegistry("hub-1", map[string]Config{
		"host-4": {Addr: server.Addr(), Token: "tok"},
	})
	t.Cleanup(registry.Close)
	registry.Probe(t.Context()) // connected, no dialer yet
	registry.SetMCPDialer(func(ctx context.Context) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "tcp", hubAddr)
	})
	endpoint, err := registry.MCPEndpoint(t.Context(), "host-4")
	if err != nil {
		t.Fatal(err)
	}
	// Wiring the dialer starts the reverse channel in a goroutine, so the
	// node answers the documented 503 until it is up. What is under test is
	// that the channel arrives on a connection that was already open, not
	// that it arrives on the first attempt.
	waitTunnel(t, endpoint)
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint, strings.NewReader(`{}`))
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("the tunnel on a connection dialed before the dialer was wired: %v", err)
	}
	defer resp.Body.Close()
	if body, _ := io.ReadAll(resp.Body); string(body) != `{"late":true}` {
		t.Fatalf("response = %d %q", resp.StatusCode, body)
	}
}
