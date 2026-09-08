package admin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agenttools"
	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
)

func startAgentAdminNode(t *testing.T, harnesses map[string]node.HarnessSpec) *node.Server {
	t.Helper()
	dir := t.TempDir()
	cfg := node.ServerConfig{Name: "node-test", Token: "test-node-token", Listen: "127.0.0.1:0", StateDir: filepath.Join(dir, "state"), Source: filepath.Join(dir, "node.json"), Harnesses: harnesses}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.Source, raw, 0600); err != nil {
		t.Fatal(err)
	}
	server := node.NewServer(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("node did not stop")
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for strings.HasSuffix(server.Addr(), ":0") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if strings.HasSuffix(server.Addr(), ":0") {
		t.Fatal("node did not start")
	}
	return server
}

func TestLocalWorkerEnrollmentRefreshesAdvertBeforeFirstSession(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "kimi")
	build := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build mockagent: %v %s", err, out)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", root)
	server := startAgentAdminNode(t, map[string]node.HarnessSpec{})
	a := agentAdminFixture(t)
	a.Cfg.Nodes = map[string]config.Node{"node-test": {Addr: server.Addr(), Token: "test-node-token"}}
	if err := config.Save(a.Path, a.Cfg); err != nil {
		t.Fatal(err)
	}
	a.Nodes = node.NewRegistry("hub-test", a.Cfg.NodeConfigs())
	t.Cleanup(a.Nodes.Close)
	before, err := a.Nodes.Advert(t.Context(), "node-test")
	if err != nil || len(before.Harnesses) != 0 {
		t.Fatalf("initial advert=%+v %v", before, err)
	}
	discovery := server.LocalAgentDiscovery()
	if _, err := server.LocalEnrollAgent(t.Context(), agenttools.InstallRequest{CandidateID: "kimi", ExpectedRevision: discovery.Revision}); err != nil {
		t.Fatal(err)
	}
	cached, _ := a.Nodes.Advert(t.Context(), "node-test")
	if len(cached.Harnesses) != 0 {
		t.Fatal("fixture unexpectedly refreshed the registry")
	}
	if err := a.AddAgent(t.Context(), consoleapi.AddAgentRequest{ID: "fresh", Harness: "kimi", Node: "node-test"}); err != nil {
		t.Fatal(err)
	}
	manager, err := harness.NewManager(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Stop)
	manager.SetTransports(a.Nodes)
	runner, err := manager.OpenSession(t.Context(), harness.Placement{Node: "node-test", Harness: "kimi"}, "", t.TempDir(), nil)
	if err != nil {
		t.Fatalf("first session after registration failed: %v", err)
	}
	out, _, err := runner.Prompt(t.Context(), "hello after registration", nil)
	if err != nil || !strings.Contains(out, "echo: hello after registration") {
		t.Fatalf("first task=%q %v", out, err)
	}
}

func waitForAgentAdminCommit(t *testing.T, method string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		buffer := make([]byte, 1<<20)
		buffer = buffer[:runtime.Stack(buffer, true)]
		for _, stack := range strings.Split(string(buffer), "\n\n") {
			if strings.Contains(stack, "(*Service)."+method+"(") && strings.Contains(stack, "lockSlow") {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("Agent mutation did not wait at its final administration commit")
}

func TestRemoteAgentMutationRejectsNodeIdentityReplacedAfterRPC(t *testing.T) {
	for _, operation := range []string{"enroll", "add", "update"} {
		t.Run(operation, func(t *testing.T) {
			a := remoteAgentAdminFixture(t)
			discovery, err := a.NodeAgents(t.Context(), "node-test")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := a.Nodes.EnrollAgent(t.Context(), "node-test", agenttools.InstallRequest{CandidateID: "kimi", ExpectedRevision: discovery.Revision}); err != nil {
				t.Fatal(err)
			}
			discovery, err = a.NodeAgents(t.Context(), "node-test")
			if err != nil {
				t.Fatal(err)
			}
			a.Mu.Lock()
			var once sync.Once
			unlock := func() { once.Do(a.Mu.Unlock) }
			defer unlock()
			done := make(chan error, 1)
			method := map[string]string{"enroll": "EnrollNodeAgent", "add": "AddAgent", "update": "UpdateAgent"}[operation]
			go func() {
				switch operation {
				case "enroll":
					_, err := a.EnrollNodeAgent(t.Context(), "node-test", agenttools.EnrollRequest{CandidateID: "kimi", AgentID: "later", ExpectedRevision: discovery.Revision})
					done <- err
				case "add":
					done <- a.AddAgent(t.Context(), consoleapi.AddAgentRequest{ID: "later", Harness: "kimi", Node: "node-test"})
				case "update":
					done <- a.UpdateAgent(t.Context(), "worker", consoleapi.AgentSpec{Harness: "kimi", Node: "node-test"})
				}
			}()
			waitForAgentAdminCommit(t, method)
			ConfigMu.Lock()
			replacement := a.Cfg.Nodes["node-test"]
			replacement.Token = "another-machine-token"
			a.Cfg.Nodes["node-test"] = replacement
			ConfigMu.Unlock()
			unlock()
			if err := <-done; !errors.Is(err, nodewire.ErrSettingsRevisionConflict) {
				t.Fatalf("old node receipt accepted for reused name: %v", err)
			}
			if _, ok := a.Cfg.Agents["later"]; ok || a.Cfg.Agents["worker"].Node != "" {
				t.Fatal("old node receipt published an Agent on a replacement machine")
			}
		})
	}
}

func remoteAgentAdminFixture(t *testing.T) *Service {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "kimi"), []byte("#!/bin/sh\nprintf 'test version\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	server := startAgentAdminNode(t, map[string]node.HarnessSpec{})
	a := agentAdminFixture(t)
	a.Cfg.Nodes = map[string]config.Node{"node-test": {Addr: server.Addr(), Token: "test-node-token"}}
	if err := config.Save(a.Path, a.Cfg); err != nil {
		t.Fatal(err)
	}
	a.Nodes = node.NewRegistry("hub-test", a.Cfg.NodeConfigs())
	t.Cleanup(a.Nodes.Close)
	return a
}

func TestRemoteAgentEnrollmentRequiresSelectionAndNoLocalHarness(t *testing.T) {
	a := remoteAgentAdminFixture(t)
	discovery, err := a.NodeAgents(t.Context(), "node-test")
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Cfg.Agents) != 2 {
		t.Fatal("discovery registered agents")
	}
	if err := a.AddAgent(t.Context(), consoleapi.AddAgentRequest{ID: "premature", Harness: "kimi", Node: "node-test"}); err == nil {
		t.Fatal("unregistered remote tool accepted")
	}
	result, err := a.EnrollNodeAgent(t.Context(), "node-test", agenttools.EnrollRequest{CandidateID: "kimi", AgentID: "remote-kimi", ExpectedRevision: discovery.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Registered || result.Harness != "kimi" || a.Cfg.Agents["remote-kimi"].Node != "node-test" {
		t.Fatalf("registration=%+v agents=%+v", result, a.Cfg.Agents)
	}
	if _, ok := a.Cfg.Harnesses["kimi"]; ok {
		t.Fatal("remote command copied into local harnesses")
	}
	if err := a.AddAgent(t.Context(), consoleapi.AddAgentRequest{ID: "remote-second", Harness: "kimi", Node: "node-test"}); err != nil {
		t.Fatal(err)
	}
	if err := a.UpdateAgent(t.Context(), "remote-second", consoleapi.AgentSpec{Harness: "kimi", Node: "node-test", Model: "node-model"}); err != nil {
		t.Fatal(err)
	}
	discovery, err = a.NodeAgents(t.Context(), "node-test")
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range discovery.Agents {
		if candidate.ID == "kimi" && (!candidate.Configured || !candidate.Registered) {
			t.Fatalf("missing registration status: %+v", candidate)
		}
	}
	if _, err := a.EnrollNodeAgent(t.Context(), "node-test", agenttools.EnrollRequest{CandidateID: "kimi", AgentID: "remote-kimi", ExpectedRevision: discovery.Revision}); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteEnrollmentCanRetryAfterCoordinatorPersistenceFailure(t *testing.T) {
	a := remoteAgentAdminFixture(t)
	discovery, err := a.NodeAgents(t.Context(), "node-test")
	if err != nil {
		t.Fatal(err)
	}
	a.WriteConfig = func(string, *config.Config) error { return errors.New("disk full") }
	result, err := a.EnrollNodeAgent(t.Context(), "node-test", agenttools.EnrollRequest{CandidateID: "kimi", AgentID: "remote-kimi", ExpectedRevision: discovery.Revision})
	if err == nil || result.Harness != "kimi" || result.Registered {
		t.Fatalf("partial registration result=%+v err=%v", result, err)
	}
	if _, ok := a.Cfg.Agents["remote-kimi"]; ok {
		t.Fatal("failed persistence published Agent")
	}
	a.WriteConfig = nil
	if _, err := a.EnrollNodeAgent(t.Context(), "node-test", agenttools.EnrollRequest{CandidateID: "kimi", AgentID: "remote-kimi", ExpectedRevision: discovery.Revision}); !errors.Is(err, nodewire.ErrSettingsRevisionConflict) {
		t.Fatalf("stale revision accepted: %v", err)
	}
	discovery, err = a.NodeAgents(t.Context(), "node-test")
	if err != nil {
		t.Fatal(err)
	}
	if result, err := a.EnrollNodeAgent(t.Context(), "node-test", agenttools.EnrollRequest{CandidateID: "kimi", AgentID: "remote-kimi", ExpectedRevision: discovery.Revision}); err != nil || !result.Registered {
		t.Fatalf("retry=%+v %v", result, err)
	}
}
