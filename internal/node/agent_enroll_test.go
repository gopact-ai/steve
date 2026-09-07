package node

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
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
	featureSet := nodewire.Features()
	r.live["node"] = &conn{name: "node", mux: mux, advert: nodewire.Advert{Node: "node", Features: featureSet}}
	t.Cleanup(r.Close)
	set := nodewire.Settings{Revision: "committed-revision", Harnesses: map[string]nodewire.HarnessSetting{"kimi": {Command: "/node/bin/kimi"}}}
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
		err = json.NewEncoder(stream).Encode(nodewire.Advert{Node: "node", Features: featureSet})
		_ = stream.Close()
		done <- err
	}()
	got, err := r.Configure(t.Context(), "node", nodewire.Settings{Revision: "before"})
	if !SettingsCommitted(err) || got.Revision != set.Revision || got.Harnesses["kimi"].Command != "/node/bin/kimi" || !refreshed.Load() {
		t.Fatalf("committed result lost: %+v err=%v refreshed=%v", got, err, refreshed.Load())
	}
	if err := <-done; err != nil {
		t.Fatal(err)
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
	if err := os.Symlink(bin, filepath.Join(toolDir, "kimi")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", toolDir)
	cfg := ServerConfig{Name: "worker", Token: "test-token", Source: filepath.Join(root, "node.json"), StateDir: filepath.Join(root, "state"), WorkspaceRoot: filepath.Join(root, "work"), Harnesses: map[string]HarnessSpec{}}
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
		if candidate.ID == "kimi" {
			found = candidate
		}
	}
	if !found.Installed || found.Configured || found.Executable != filepath.Join(toolDir, "kimi") {
		t.Fatalf("discovery=%+v", found)
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "runtimes")); !os.IsNotExist(err) {
		t.Fatalf("empty node touched unselected runtime: %v", err)
	}
	result, err := registry.EnrollAgent(t.Context(), "worker", agenttools.InstallRequest{CandidateID: "kimi", ExpectedRevision: discovery.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if result.Harness != "kimi" || result.Revision == discovery.Revision {
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
	if len(parsed.Harnesses) != 1 || parsed.Harnesses["kimi"].Command != found.Executable || len(parsed.Harnesses["kimi"].Env) != 0 {
		t.Fatalf("registered unexpected config: %+v", parsed.Harnesses)
	}
	broker, _ := permission.New(permission.PolicyRead)
	host := acphost.New(acphost.Config{Transport: registry.Transport("worker", "kimi"), Permission: broker})
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
	if _, err := stream.Write([]byte(`{"candidate_id":"kimi","expected_revision":"x","command":"touch /tmp/should-not-run"}` + "\n")); err != nil {
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
