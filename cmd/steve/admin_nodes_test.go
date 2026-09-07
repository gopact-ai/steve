package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/roster"
)

func nodeAdminFixture(t *testing.T) *fleetAdmin {
	t.Helper()
	admin := agentAdminFixture(t)
	admin.cfg.Nodes = map[string]config.Node{"node-test": {Addr: "127.0.0.1:1", Token: "test-node-token", Level: "internal"}}
	if err := config.Save(admin.path, admin.cfg); err != nil {
		t.Fatal(err)
	}
	admin.nodes = node.NewRegistry("hub-test", admin.cfg.NodeConfigs())
	t.Cleanup(admin.nodes.Close)
	admin.fleet = roster.New(admin.catalog)
	return admin
}

// Observe the removal waiting on the administration mutex while another
// operation owns it. That fixes the interleaving without timing a disk write.
func waitForBlockedNodeRemoval(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		raw := make([]byte, 1<<20)
		raw = raw[:runtime.Stack(raw, true)]
		for _, stack := range strings.Split(string(raw), "\n\n") {
			if strings.Contains(stack, "(*fleetAdmin).RemoveNode(") && strings.Contains(stack, "lockSlow") {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("node removal did not wait for the pending administration operation")
}

func TestRemoveNodeRechecksAgentPlacementAfterPendingUpdate(t *testing.T) {
	admin := nodeAdminFixture(t)
	// Placement now checks the destination's own tool declaration. Keep the
	// original serialization test against a real authenticated node.
	server := startAgentAdminNode(t, map[string]node.HarnessSpec{"mock": {Command: "/bin/echo"}})
	admin.cfg.Nodes["node-test"] = config.Node{Addr: server.Addr(), Token: "test-node-token", Level: "internal"}
	admin.nodes.Add("node-test", node.Config{Addr: server.Addr(), Token: "test-node-token", Level: "internal"})
	if err := config.Save(admin.path, admin.cfg); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	admin.writeConfig = func(path string, candidate *config.Config) error {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
		return config.Save(path, candidate)
	}
	updated := make(chan error, 1)
	go func() {
		updated <- admin.UpdateAgent(t.Context(), "worker", consoleapi.AgentSpec{Harness: "mock", Node: "node-test"})
	}()
	<-entered
	removed := make(chan error, 1)
	go func() { removed <- admin.RemoveNode(t.Context(), "node-test") }()
	waitForBlockedNodeRemoval(t)
	unblock()
	if err := <-updated; err != nil {
		t.Fatal(err)
	}
	if err := <-removed; err == nil || !strings.Contains(err.Error(), "Agent worker") {
		t.Fatalf("removed a node acquired by the pending update: %v", err)
	}
	if _, ok := admin.cfg.Nodes["node-test"]; !ok {
		t.Fatal("occupied node removed from configuration")
	}
	if len(admin.nodes.Names()) != 1 {
		t.Fatal("occupied node removed from runtime registry")
	}
}

func TestNodeMutationsSynchronizeWithConfigurationReaders(t *testing.T) {
	admin := nodeAdminFixture(t)
	ctx, stop := context.WithCancel(context.Background())
	var readers sync.WaitGroup
	readers.Add(1)
	go func() {
		defer readers.Done()
		for ctx.Err() == nil {
			configMu.RLock()
			_, _ = json.Marshal(admin.cfg)
			_ = admin.cfg.NodeLevels()
			_ = admin.cfg.NodeRegions()
			configMu.RUnlock()
			runtime.Gosched()
		}
	}()
	defer func() { stop(); readers.Wait() }()
	for i := 0; i < 12; i++ {
		name := fmt.Sprintf("node-concurrent-%d", i)
		if _, err := admin.AddNode(t.Context(), consoleapi.AddNodeRequest{Name: name, Addr: "127.0.0.1:1"}); err != nil {
			t.Fatal(err)
		}
		if err := admin.RemoveNode(t.Context(), name); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRemoveNodeRechecksProjectPlacementAfterPendingDeclaration(t *testing.T) {
	admin := nodeAdminFixture(t)
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = book.Close() })
	admin.projects = project.Open(book)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	admin.writeConfig = func(path string, candidate *config.Config) error {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
		return config.Save(path, candidate)
	}
	declared := make(chan error, 1)
	go func() {
		declared <- admin.AddProject(t.Context(), consoleapi.AddProjectRequest{ID: "project-test", Node: "node-test", Path: "/test/node-project"})
	}()
	<-entered
	removed := make(chan error, 1)
	go func() { removed <- admin.RemoveNode(t.Context(), "node-test") }()
	waitForBlockedNodeRemoval(t)
	unblock()
	if err := <-declared; err != nil {
		t.Fatal(err)
	}
	if err := <-removed; err == nil || !strings.Contains(err.Error(), "项目 project-test") {
		t.Fatalf("removed a node acquired by the pending declaration: %v", err)
	}
	if _, ok := admin.cfg.Nodes["node-test"]; !ok {
		t.Fatal("project's node removed from configuration")
	}
	if len(admin.nodes.Names()) != 1 {
		t.Fatal("project's node removed from runtime registry")
	}
}

func TestRemoveNodeRefusesUnknownProjectOccupancy(t *testing.T) {
	admin := nodeAdminFixture(t)
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	admin.projects = project.Open(book)
	if err := book.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(admin.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.RemoveNode(t.Context(), "node-test"); err == nil {
		t.Fatal("removed a node without knowing whether projects use it")
	}
	after, err := os.ReadFile(admin.path)
	if err != nil || string(before) != string(after) {
		t.Fatal("failed occupancy check changed persistent configuration")
	}
	if len(admin.nodes.Names()) != 1 {
		t.Fatal("failed occupancy check removed the runtime node")
	}
}
