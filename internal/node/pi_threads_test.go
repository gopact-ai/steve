package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/pihome"
	steveruntime "github.com/gopact-ai/steve/internal/runtime"
)

func startSessionServer(t *testing.T, cfg ServerConfig) *Server {
	t.Helper()
	if cfg.SessionAuthorizer == nil {
		cfg.SessionAuthorizer = &sessionAuthorityTest{epoch: 1, writer: 1}
	}
	s := NewServer(cfg)
	if err := s.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.sessions.Close)
	return s
}

// A node whose configuration still names a retired tool never starts it:
// not for a node-owned open, not for a capability probe.
func TestNodeNeverStartsARetiredHarness(t *testing.T) {
	bin := buildMockAgent(t)
	s := startSessionServer(t, ServerConfig{Name: "worker", StateDir: t.TempDir(), WorkspaceRoot: t.TempDir(), Harnesses: map[string]HarnessSpec{"kimi": {Command: bin}, "dsh": {Command: bin}}})
	for _, id := range []string{"kimi", "dsh"} {
		req := nodeSessionRequest(nodewire.SessionActionOpen)
		req.Harness, req.Workdir, req.CommandID = id, t.TempDir(), id+"/open"
		_, err := s.sessions.Do(t.Context(), "cluster-1", req)
		var refused *nodewire.SessionOpenNotStarted
		if !errors.As(err, &refused) || !strings.Contains(err.Error(), "no longer supported") {
			t.Fatalf("%s open = %v", id, err)
		}
		probe := nodeSessionRequest(nodewire.SessionActionCapabilities)
		probe.Harness = id
		if _, err := s.sessions.Do(t.Context(), "cluster-1", probe); err == nil || !strings.Contains(err.Error(), "no longer supported") {
			t.Fatalf("%s probe = %v", id, err)
		}
	}
}

// The capability probe runs pi from Steve's read-only home; an operator's
// own PI_CODING_AGENT_DIR is neither used nor a reason to refuse.
func TestNodePiCapabilityProbeIgnoresAnOperatorAgentDir(t *testing.T) {
	bin := buildMockAgent(t)
	t.Setenv("HOME", t.TempDir())
	s := startSessionServer(t, ServerConfig{Name: "worker", StateDir: t.TempDir(), WorkspaceRoot: t.TempDir(), Harnesses: map[string]HarnessSpec{
		harness.Pi: {Command: bin, Env: []string{"PI_CODING_AGENT_DIR=" + t.TempDir()}},
	}})
	probe := nodeSessionRequest(nodewire.SessionActionCapabilities)
	probe.Harness = harness.Pi
	if _, err := s.sessions.Do(t.Context(), "cluster-1", probe); err != nil {
		t.Fatalf("probe refused: %v", err)
	}
}

// pi's home failing its check before anything is reserved is a definite
// refusal the hub can retry, not an uncertain open.
func TestNodePiLaunchRefusalIsNotStarted(t *testing.T) {
	bin := buildMockAgent(t)
	t.Setenv("HOME", t.TempDir())
	state := t.TempDir()
	// A real directory where the shared skills link belongs: the home
	// cannot be prepared.
	if err := os.MkdirAll(filepath.Join(pihome.Dir(steveruntime.PiHome(state), pihome.ReadOnly), "skills"), 0o700); err != nil {
		t.Fatal(err)
	}
	s := startSessionServer(t, ServerConfig{Name: "worker", StateDir: state, WorkspaceRoot: t.TempDir(), Harnesses: map[string]HarnessSpec{harness.Pi: {Command: bin}}})
	req := nodeSessionRequest(nodewire.SessionActionOpen)
	req.Harness, req.Workdir, req.CommandID, req.Permission = harness.Pi, t.TempDir(), "pi/open", "read"
	_, err := s.sessions.Do(t.Context(), "cluster-1", req)
	var refused *nodewire.SessionOpenNotStarted
	if !errors.As(err, &refused) {
		t.Fatalf("pi launch refusal = %v (%T)", err, err)
	}
}

// A node-owned pi session with a plugin runtime runs in the runtime's home
// of the class its policy needs, through that home's launcher.
func TestNodeOwnedPiPluginSessionFollowsItsPolicy(t *testing.T) {
	bin := buildMockAgent(t)
	t.Setenv("HOME", t.TempDir())
	for policy, class := range map[string]string{"write": pihome.Full, "read": pihome.ReadOnly} {
		s := startSessionServer(t, ServerConfig{Name: "worker", StateDir: t.TempDir(), Harnesses: map[string]HarnessSpec{harness.Pi: {Command: bin}}})
		selection := nodeRuntimeFixture(t, s, "")
		selection.Harness = harness.Pi
		runtime, err := s.pluginRuntimePool().Prepare(t.Context(), "native-"+policy, selection, harness.Config{Command: bin, Permission: policy})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(s.closePluginRuntimes)
		req := nodeSessionRequest(nodewire.SessionActionOpen)
		req.Harness, req.Workdir, req.CommandID, req.Permission = harness.Pi, t.TempDir(), "open-"+policy, policy
		req.Plugin = &runtime.Ref
		req.Binding.PluginRuntimeID = runtime.Ref.ID
		opened, err := s.sessions.Do(t.Context(), "cluster-1", req)
		if err != nil {
			t.Fatal(err)
		}
		out := promptOwned(t, s, req, opened, "mockenv PI_CODING_AGENT_DIR mockenv PI_ACP_PI_COMMAND")
		home := filepath.Join(s.pluginStore().RuntimeDir(runtime.Ref.ID), "home", class)
		if !strings.Contains(out, "env PI_CODING_AGENT_DIR="+home+"\n") || !strings.Contains(out, "env PI_ACP_PI_COMMAND="+filepath.Join(home, "steve-pi")) {
			t.Fatalf("%s plugin session ran with %q, want %s", policy, out, home)
		}
	}
}

func promptOwned(t *testing.T, s *Server, req nodewire.SessionRequest, opened nodewire.SessionState, text string) string {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req.ID, req.Action, req.InputSequence, req.Text = opened.ID, nodewire.SessionActionPrompt, 1, text
	req.CommandID = "input-1"
	state, err := s.sessions.Do(ctx, "cluster-1", req)
	if err != nil {
		t.Fatal(err)
	}
	for range 200 {
		if state.Command != nil && state.Command.ID == req.CommandID && state.Command.Settled {
			return state.Command.Output
		}
		poll := req
		poll.Action, poll.After, poll.WaitMS = nodewire.SessionActionPoll, state.Sequence, 100
		if state, err = s.sessions.Do(ctx, "cluster-1", poll); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatalf("prompt %q did not settle: %+v", text, state.Command)
	return ""
}

// A remote pi plugin session is not checked against the hub's own pi home:
// the node chooses the runtime's home from the policy the stream carries.
func TestRemotePiPluginSessionGetsItsHomeOnTheNode(t *testing.T) {
	bin := buildMockAgent(t)
	t.Setenv("HOME", t.TempDir())
	s := startNode(t, ServerConfig{Name: "worker", Token: "plugin", StateDir: t.TempDir(), Harnesses: map[string]HarnessSpec{harness.Pi: {Command: bin}}})
	registry := NewRegistry("owner", map[string]Config{"worker": {Addr: s.Addr(), Token: "plugin"}})
	t.Cleanup(registry.Close)
	selection := nodeRuntimeFixture(t, s, "")
	selection.Harness = harness.Pi
	prepared, err := registry.Plugins(t.Context(), "worker", nodewire.PluginRequest{Action: nodewire.PluginRuntimePrepare, Selection: &selection, CommandID: "remote-pi"})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := harness.NewManager(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Stop)
	manager.SetTransports(registry)
	manager.SetPluginRuntimes(nodePluginProvider{registry})
	session, err := manager.OpenSession(harness.WithPluginProfile(t.Context(), prepared.Runtime), harness.Placement{Node: "worker", Harness: harness.Pi}, "", t.TempDir(), nil)
	if err != nil {
		t.Fatalf("remote pi plugin session refused on the hub: %v", err)
	}
	answer, _, err := session.Prompt(t.Context(), "mockenv PI_CODING_AGENT_DIR", nil)
	home := filepath.Join(s.pluginStore().RuntimeDir(prepared.Runtime.ID), "home", pihome.ReadOnly)
	if err != nil || !strings.Contains(answer, "env PI_CODING_AGENT_DIR="+home) {
		t.Fatalf("remote pi plugin session ran with %q (%v), want %s", answer, err, home)
	}
}
