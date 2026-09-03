package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/gopact-ai/steve/internal/nodewire"
	steveruntime "github.com/gopact-ai/steve/internal/runtime"
	"github.com/gopact-ai/steve/internal/skills"
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
	for _, dest := range steveruntime.SkillDests(state) {
		if _, err := os.ReadFile(filepath.Join(dest, "deploy", "SKILL.md")); err != nil {
			t.Errorf("skill not materialized in %s: %v", dest, err)
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
	server := startNode(t, ServerConfig{
		Name: "host-9", Token: "tok", StateDir: state,
		Harnesses: map[string]HarnessSpec{"codex": {Command: bin}},
		MCPServers: map[string]MCPSpec{
			"echo": {Type: "stdio", Command: "sh", Args: []string{"-c", `read line; echo "got $line via $TOKEN"`}, Env: map[string]string{"TOKEN": "SECRET-42"}},
			"web":  {Type: "http", URL: "http://127.0.0.1:1/mcp"},
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
	if echo.Availability != ability.Available || web.Availability != ability.Unknown {
		t.Fatalf("echo = %s, web = %s; stdio binds, http does not yet", echo.Availability, web.Availability)
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
	adm, _ = registry.Admit(t.Context(), "host-9", nodewire.AdmitRequest{Attempt: "a-http", Harness: "codex", Requirement: req, Uses: []string{"web"}})
	if !adm.Refused() || adm.Code != ability.CodeUnavailable {
		t.Fatalf("an http server should refuse until the node proxies it: %+v", adm)
	}
}

// A node remembers its hub. While that hub is silent but within grace a
// second hub is refused, so a blip cannot hand the machine to whoever
// dials next; a hub that disconnected cleanly has given the node back;
// adopt hands it over explicitly.
func TestNodeRemembersItsHub(t *testing.T) {
	bin := buildMockAgent(t)
	state := t.TempDir()
	server := startNode(t, ServerConfig{Name: "host-10", Token: "tok", StateDir: state, Harnesses: map[string]HarnessSpec{"codex": {Command: bin}}})
	first := NewRegistry("hub-1", map[string]Config{"host-10": {Addr: server.Addr(), Token: "tok"}})
	if _, err := first.Advert(t.Context(), "host-10"); err != nil {
		t.Fatal(err)
	}
	// The owner went silent: pretend by rewriting the record it wrote.
	first.Close()
	time.Sleep(100 * time.Millisecond)
	server.writeOwner(hubOwner{Hub: "hub-1", LastSeen: time.Now().Add(-time.Minute)})
	second := NewRegistry("hub-2", map[string]Config{"host-10": {Addr: server.Addr(), Token: "tok"}})
	t.Cleanup(second.Close)
	if _, err := second.Advert(t.Context(), "host-10"); err == nil || !errors.Is(err, nodewire.ErrRefused) {
		t.Fatalf("a second hub took over a node whose hub went silent a minute ago: %v", err)
	}
	// Beyond grace, or after a clean release, or by adoption, it is free.
	server.writeOwner(hubOwner{Hub: "hub-1", LastSeen: time.Now().Add(-OwnerGrace - time.Minute)})
	if _, err := second.Advert(t.Context(), "host-10"); err != nil {
		t.Fatalf("after grace: %v", err)
	}
	second.Close()
	time.Sleep(100 * time.Millisecond)
	if o := server.owner(); o.Hub != "hub-2" || !o.Released {
		t.Fatalf("owner after a clean disconnect = %+v", o)
	}
	third := NewRegistry("hub-3", map[string]Config{"host-10": {Addr: server.Addr(), Token: "tok"}})
	t.Cleanup(third.Close)
	if _, err := third.Advert(t.Context(), "host-10"); err != nil {
		t.Fatalf("after a clean release: %v", err)
	}
	third.Close()
	time.Sleep(100 * time.Millisecond)
	server.writeOwner(hubOwner{Hub: "hub-3", LastSeen: time.Now()})
	if err := Adopt(state, "hub-4"); err != nil {
		t.Fatal(err)
	}
	fourth := NewRegistry("hub-4", map[string]Config{"host-10": {Addr: server.Addr(), Token: "tok"}})
	t.Cleanup(fourth.Close)
	if _, err := fourth.Advert(t.Context(), "host-10"); err != nil {
		t.Fatalf("after adopt: %v", err)
	}
}
