package node

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/adapter"
	"github.com/gopact-ai/steve/internal/agenttools"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/permission"
)

type fakeAgentInstaller struct {
	ensure func(context.Context, string) (adapter.Installed, error)
}

func TestConfigurePreservesCommittedSettingsErrorAndRefreshes(t *testing.T) {
	client, server := net.Pipe()
	mux := nodewire.NewMux(client, true)
	remote := nodewire.NewMux(server, false)
	t.Cleanup(func() { _ = remote.Close() })
	r := NewRegistry("hub", map[string]Config{"node": {Addr: "test", Token: "test"}})
	r.live["node"] = &conn{name: "node", mux: mux, advert: nodewire.Advert{Node: "node"}}
	t.Cleanup(r.Close)
	set := nodewire.Settings{Revision: "committed-revision", Harnesses: map[string]nodewire.HarnessSetting{"pi": {Command: "/node/bin/pi"}}}
	var refreshed atomic.Bool
	done := make(chan error, 1)
	go func() {
		stream, err := remote.Accept(t.Context())
		if err != nil {
			done <- err
			return
		}
		var request nodewire.Settings
		if err = json.NewDecoder(stream).Decode(&request); err == nil {
			err = json.NewEncoder(stream).Encode(nodewire.ConfigReply{Settings: set, ErrorCode: "settings_committed", Error: "directory sync failed"})
		}
		_ = stream.Close()
		if err != nil {
			done <- err
			return
		}
		stream, err = remote.Accept(t.Context())
		if err != nil {
			done <- err
			return
		}
		refreshed.Store(stream.Request().Kind == nodewire.StreamAdvert)
		err = json.NewEncoder(stream).Encode(nodewire.Advert{Node: "node"})
		_ = stream.Close()
		done <- err
	}()
	got, err := r.Configure(t.Context(), "node", nodewire.Settings{Revision: "before"})
	if !SettingsCommitted(err) || got.Revision != set.Revision || got.Harnesses["pi"].Command != "/node/bin/pi" || !refreshed.Load() {
		t.Fatalf("committed result lost: %+v err=%v refreshed=%v", got, err, refreshed.Load())
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A reply without a discovery revision is a node that returned no
// discovery, and the error says so.
func TestAgentToolsReportsAReplyWithoutDiscovery(t *testing.T) {
	client, server := net.Pipe()
	mux := nodewire.NewMux(client, true)
	remote := nodewire.NewMux(server, false)
	t.Cleanup(func() { _ = remote.Close() })
	r := NewRegistry("hub", map[string]Config{"node": {Addr: "test", Token: "test"}})
	r.live["node"] = &conn{name: "node", mux: mux, advert: nodewire.Advert{Node: "node"}}
	t.Cleanup(r.Close)
	go func() {
		stream, err := remote.Accept(t.Context())
		if err != nil {
			return
		}
		_ = json.NewEncoder(stream).Encode(agentToolsReply{})
		_ = stream.Close()
	}()
	_, err := r.AgentTools(t.Context(), "node")
	if err == nil || err.Error() != `node "node" returned no agent discovery` {
		t.Fatalf("agent tools = %v", err)
	}
}

func TestNodeAgentEnrollmentOverAuthenticatedTransportKeepsToolsNodeLocal(t *testing.T) {
	bin := buildMockAgent(t)
	root := t.TempDir()
	t.Setenv("HOME", root)
	toolDir := filepath.Join(root, "bin")
	if err := os.Mkdir(toolDir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pi", "node", "npm"} {
		if err := os.WriteFile(filepath.Join(toolDir, name), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", toolDir)
	cfg := ServerConfig{Name: "worker", Token: "test-token", Source: filepath.Join(root, "node.json"), StateDir: filepath.Join(root, "state"), WorkspaceRoot: filepath.Join(root, "work"), Harnesses: map[string]HarnessSpec{}}
	// The pinned pi-acp is already in this machine's adapter cache; here it
	// is the mock agent, so the enrolled tool can serve a prompt.
	cachedNodeAdapter(t, filepath.Join(cfg.StateDir, "adapters"), "pi-acp", "#!/bin/sh\nexec '"+bin+"' \"$@\"\n")
	if err := writeConfig(cfg); err != nil {
		t.Fatal(err)
	}
	server := startNode(t, cfg)
	registry := NewRegistry("coordinator", map[string]Config{"worker": {Addr: server.Addr(), Token: "test-token"}})
	t.Cleanup(registry.Close)
	discovery, err := registry.AgentTools(t.Context(), "worker")
	if err != nil {
		t.Fatal(err)
	}
	var found agenttools.Candidate
	for _, candidate := range discovery.Agents {
		if candidate.ID == "pi" {
			found = candidate
		}
	}
	if !found.Installed || found.Configured || found.Executable != filepath.Join(toolDir, "pi") {
		t.Fatalf("discovery=%+v", found)
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "runtimes")); !os.IsNotExist(err) {
		t.Fatalf("empty node touched unselected runtime: %v", err)
	}
	result, err := registry.EnrollAgent(t.Context(), "worker", agenttools.InstallRequest{CandidateID: "pi", ExpectedRevision: discovery.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if result.Harness != "pi" || result.Revision == discovery.Revision {
		t.Fatalf("enrollment=%+v", result)
	}
	stored, err := os.ReadFile(cfg.Source)
	if err != nil {
		t.Fatal(err)
	}
	var parsed ServerConfig
	if err := json.Unmarshal(stored, &parsed); err != nil {
		t.Fatal(err)
	}
	enrolled := parsed.Harnesses["pi"]
	// The adapter's own path is resolved from the cache at start; only the
	// pi it drives is recorded.
	if len(parsed.Harnesses) != 1 || enrolled.Adapter != "pi-acp" || enrolled.Command != "" || !slices.Equal(enrolled.Env, []string{"PI_ACP_PI_COMMAND=" + found.Executable}) {
		t.Fatalf("registered unexpected config: %+v", parsed.Harnesses)
	}
	broker, _ := permission.New(permission.PolicyRead)
	host := acphost.New(acphost.Config{Transport: registry.Transport("worker", "pi"), Permission: broker})
	t.Cleanup(host.Stop)
	id, generation, err := host.OpenSession(t.Context(), "", acphost.SessionConfig{Workdir: cfg.WorkspaceRoot})
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := host.Prompt(t.Context(), id, generation, "hello", nil)
	if err != nil || !strings.Contains(out, "echo: hello") {
		t.Fatalf("registered tool cannot serve: %q %v", out, err)
	}
}

func TestEnrollmentLeavesExistingConfigurationAndRejectsInjectedCommand(t *testing.T) {
	s, deps := enrollmentFixture(t)
	set := s.settings()
	set.Harnesses["codex"] = nodewire.HarnessSetting{Command: "custom-cli", Env: []string{"SECRET=keep"}}
	if err := s.applySettings(set); err != nil {
		t.Fatal(err)
	}
	if _, err := s.enrollAgentWith(t.Context(), agenttools.InstallRequest{CandidateID: "codex", ExpectedRevision: s.settings().Revision}, deps); !errors.Is(err, agenttools.ErrConfigured) {
		t.Fatalf("replaced preexisting adapter: %v", err)
	}
	if s.conf().Harnesses["codex"].Command != "custom-cli" {
		t.Fatal("existing tool was replaced")
	}
	if _, err := s.enrollAgentWith(t.Context(), agenttools.InstallRequest{CandidateID: "codex; touch /tmp/no", ExpectedRevision: s.settings().Revision}, deps); !errors.Is(err, agenttools.ErrUnsupported) {
		t.Fatalf("invalid candidate accepted: %v", err)
	}
}

func TestEnrollmentDetectsExternalFileEditsAndDoesNotInstallOnConflict(t *testing.T) {
	s, deps := enrollmentFixture(t)
	revision := s.settings().Revision
	raw, err := os.ReadFile(s.conf().Source)
	if err != nil {
		t.Fatal(err)
	}
	changed := append(raw, '\n')
	if err := os.WriteFile(s.conf().Source, changed, 0600); err != nil {
		t.Fatal(err)
	}
	deps.installer = fakeAgentInstaller{ensure: func(context.Context, string) (adapter.Installed, error) {
		t.Fatal("conflict started installation")
		return adapter.Installed{}, nil
	}}
	if _, err := s.enrollAgentWith(t.Context(), agenttools.InstallRequest{CandidateID: "codex", ExpectedRevision: revision}, deps); !errors.Is(err, nodewire.ErrSettingsRevisionConflict) {
		t.Fatalf("external edit accepted: %v", err)
	}
	got, _ := os.ReadFile(s.conf().Source)
	if string(got) != string(changed) || len(s.conf().Harnesses) != 0 {
		t.Fatal("conflicting enrollment changed node settings")
	}
}

func TestEnrollmentRPCRejectsClientCommands(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	server := startNode(t, ServerConfig{Name: "worker", Token: "token", StateDir: t.TempDir(), Harnesses: map[string]HarnessSpec{}})
	registry := NewRegistry("hub", map[string]Config{"worker": {Addr: server.Addr(), Token: "token"}})
	t.Cleanup(registry.Close)
	c, err := registry.connect(t.Context(), "worker")
	if err != nil {
		t.Fatal(err)
	}
	stream, err := c.mux.Open(nodewire.OpenRequest{Kind: nodewire.StreamConfig, Command: "enroll-agent"})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if _, err := stream.Write([]byte(`{"candidate_id":"pi","expected_revision":"x","command":"touch /tmp/should-not-run"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	var reply agentToolsReply
	if err := json.NewDecoder(stream).Decode(&reply); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply.Error, "unknown field") || reply.Enrollment != nil {
		t.Fatalf("arbitrary command request accepted: %+v", reply)
	}
}

func (f fakeAgentInstaller) Ensure(ctx context.Context, id string) (adapter.Installed, error) {
	return f.ensure(ctx, id)
}

func enrollmentFixture(t *testing.T) (*Server, enrollDependencies) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"codex", "node", "npm"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nexit 99\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	cfg := ServerConfig{Name: "test", Token: "token", Source: filepath.Join(dir, "node.json"), StateDir: filepath.Join(dir, "state"), Harnesses: map[string]HarnessSpec{}}
	if err := writeConfig(cfg); err != nil {
		t.Fatal(err)
	}
	s := NewServer(cfg)
	pin := adapter.Catalog["codex-acp"]
	deps := enrollDependencies{discover: func() []agenttools.Candidate { return agenttools.Discover(agenttools.Options{Path: bin}) }, prepare: func(string, []string) error { return nil }, installer: fakeAgentInstaller{ensure: func(context.Context, string) (adapter.Installed, error) {
		return adapter.Installed{Name: "codex-acp", Package: pin.Package, Version: pin.Version, Command: filepath.Join(bin, "codex")}, nil
	}}}
	return s, deps
}

func TestEnrollmentInstallsOnlySelectedPinnedAdapterThenPublishes(t *testing.T) {
	s, deps := enrollmentFixture(t)
	before := s.settings()
	deps.installer = fakeAgentInstaller{ensure: func(_ context.Context, id string) (adapter.Installed, error) {
		if id != "codex-acp" || len(s.conf().Harnesses) != 0 {
			t.Fatal("wrong adapter or published before install")
		}
		pin := adapter.Catalog[id]
		return adapter.Installed{Name: id, Package: pin.Package, Version: pin.Version, Command: deps.discover()[0].Executable}, nil
	}}
	got, err := s.enrollAgentWith(t.Context(), agenttools.InstallRequest{CandidateID: "codex", ExpectedRevision: before.Revision}, deps)
	if err != nil {
		t.Fatal(err)
	}
	if got.Harness != "codex" || got.Revision == before.Revision {
		t.Fatalf("enrollment=%+v", got)
	}
	h := s.conf().Harnesses["codex"]
	if h.Adapter != "codex-acp" || h.Command == "" || len(h.Env) != 0 {
		t.Fatalf("node declaration=%+v", h)
	}
	if err := s.applySettings(s.settings()); err != nil {
		t.Fatal(err)
	}
}

func TestEnrollmentRejectsFailedWrongVersionAndStaleInstallWithoutPublishing(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"failed", "wrong-version", "stale"} {
		t.Run(kind, func(t *testing.T) {
			s, deps := enrollmentFixture(t)
			before := s.settings()
			deps.installer = fakeAgentInstaller{ensure: func(_ context.Context, id string) (adapter.Installed, error) {
				if kind == "failed" {
					return adapter.Installed{}, errors.New("offline")
				}
				pin := adapter.Catalog[id]
				version := pin.Version
				if kind == "wrong-version" {
					version = "untrusted"
				}
				if kind == "stale" {
					set := s.settings()
					set.Tools = []string{"git"}
					if err := s.applySettings(set); err != nil {
						t.Fatal(err)
					}
				}
				return adapter.Installed{Name: id, Package: pin.Package, Version: version, Command: deps.discover()[0].Executable}, nil
			}}
			_, err := s.enrollAgentWith(t.Context(), agenttools.InstallRequest{CandidateID: "codex", ExpectedRevision: before.Revision}, deps)
			if err == nil || len(s.conf().Harnesses) != 0 {
				t.Fatalf("rejected install published: %v %+v", err, s.conf().Harnesses)
			}
			if kind == "stale" && !errors.Is(err, nodewire.ErrSettingsRevisionConflict) {
				t.Fatalf("stale error=%v", err)
			}
		})
	}
}

// cachedNodeAdapter places a verified installation of the pinned adapter
// name running script, the shape adapter.Installer leaves behind.
func cachedNodeAdapter(t *testing.T, dir, name, script string) string {
	t.Helper()
	pin := adapter.Catalog[name]
	installed := filepath.Join(dir, name+"@"+pin.Version)
	command := filepath.Join(installed, pin.Bin)
	if err := os.MkdirAll(filepath.Dir(command), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(command, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	marker, _ := json.Marshal(map[string]string{"package": pin.Package, "version": pin.Version, "integrity": pin.Integrity})
	if err := os.WriteFile(filepath.Join(installed, ".steve-adapter.json"), marker, 0o600); err != nil {
		t.Fatal(err)
	}
	return command
}

// pi moved (reinstalled elsewhere): enrolling it again updates the stored
// path instead of refusing, and keeps the operator's other variables.
func TestEnrollmentUpdatesAMovedPi(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"pi", "node", "npm"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	piACP := filepath.Join(dir, "pi-acp")
	pin := adapter.Catalog["pi-acp"]
	start := func(t *testing.T, spec HarnessSpec) (*Server, enrollDependencies) {
		cfg := ServerConfig{Name: "test", Token: "token", Source: filepath.Join(t.TempDir(), "node.json"), StateDir: t.TempDir(), Harnesses: map[string]HarnessSpec{"pi": spec}}
		if err := writeConfig(cfg); err != nil {
			t.Fatal(err)
		}
		deps := enrollDependencies{discover: func() []agenttools.Candidate { return agenttools.Discover(agenttools.Options{Path: dir}) }, prepare: func(string, []string) error { return nil }, installer: fakeAgentInstaller{ensure: func(context.Context, string) (adapter.Installed, error) {
			return adapter.Installed{Name: "pi-acp", Package: pin.Package, Version: pin.Version, Command: piACP}, nil
		}}}
		if err := os.WriteFile(piACP, []byte("#!/bin/sh\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		return NewServer(cfg), deps
	}
	work := t.TempDir()
	s, deps := start(t, HarnessSpec{Adapter: "pi-acp", Command: piACP, Env: []string{"KEEP=1", "PI_ACP_PI_COMMAND=/old/place/pi"}, Slots: 2, Models: []string{"relay/m1"}, ProcessDir: work})
	got, err := s.enrollAgentWith(t.Context(), agenttools.InstallRequest{CandidateID: "pi", ExpectedRevision: s.settings().Revision}, deps)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"KEEP=1", "PI_ACP_PI_COMMAND=" + filepath.Join(dir, "pi")}
	updated := s.conf().Harnesses["pi"]
	if !slices.Equal(updated.Env, want) || got.Harness != "pi" {
		t.Fatalf("moved pi = %v (%+v)", updated.Env, got)
	}
	// The rest of the entry is the operator's and stays.
	if updated.Slots != 2 || !slices.Equal(updated.Models, []string{"relay/m1"}) || updated.ProcessDir != work || updated.Adapter != "pi-acp" || updated.Command != piACP {
		t.Fatalf("moved pi lost its settings: %+v", updated)
	}
	stored, _ := os.ReadFile(s.conf().Source)
	if !strings.Contains(string(stored), want[1]) {
		t.Fatalf("moved pi not saved: %s", stored)
	}
	// Same path again is a no-op.
	if _, err := s.enrollAgentWith(t.Context(), agenttools.InstallRequest{CandidateID: "pi", ExpectedRevision: s.settings().Revision}, deps); err != nil {
		t.Fatal(err)
	}
	// An own pi-acp build stays the operator's.
	own, deps := start(t, HarnessSpec{Command: "/own/pi-acp", Env: []string{"PI_ACP_PI_COMMAND=/old/pi"}})
	if _, err := own.enrollAgentWith(t.Context(), agenttools.InstallRequest{CandidateID: "pi", ExpectedRevision: own.settings().Revision}, deps); !errors.Is(err, agenttools.ErrConfigured) {
		t.Fatalf("own pi-acp replaced: %v", err)
	}
}
