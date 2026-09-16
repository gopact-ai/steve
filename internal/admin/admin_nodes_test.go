package admin

import (
	"context"
	"encoding/json"
	"errors"
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

func nodeAdminFixture(t *testing.T) *Service {
	t.Helper()
	admin := agentAdminFixture(t)
	admin.Cfg.Nodes = map[string]config.Node{"node-test": {Addr: "127.0.0.1:1", Token: "test-node-token", Level: "internal"}}
	if err := config.Save(admin.Path, admin.Cfg); err != nil {
		t.Fatal(err)
	}
	admin.Nodes = node.NewRegistry("hub-test", admin.Cfg.NodeConfigs())
	t.Cleanup(admin.Nodes.Close)
	admin.Fleet = roster.New(admin.Catalog)
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
			if strings.Contains(stack, "(*Service).RemoveNode(") && strings.Contains(stack, "lockSlow") {
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
	admin.Cfg.Nodes["node-test"] = config.Node{Addr: server.Addr(), Token: "test-node-token", Level: "internal"}
	admin.Nodes.Add("node-test", node.Config{Addr: server.Addr(), Token: "test-node-token", Level: "internal"})
	if err := config.Save(admin.Path, admin.Cfg); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	admin.WriteConfig = func(path string, candidate *config.Config) error {
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
	if _, ok := admin.Cfg.Nodes["node-test"]; !ok {
		t.Fatal("occupied node removed from configuration")
	}
	if len(admin.Nodes.Names()) != 1 {
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
			ConfigMu.RLock()
			_, _ = json.Marshal(admin.Cfg)
			_ = admin.Cfg.NodeLevels()
			_ = admin.Cfg.NodeRegions()
			ConfigMu.RUnlock()
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
	admin.Projects = project.Open(book)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	admin.WriteConfig = func(path string, candidate *config.Config) error {
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
	if _, ok := admin.Cfg.Nodes["node-test"]; !ok {
		t.Fatal("project's node removed from configuration")
	}
	if len(admin.Nodes.Names()) != 1 {
		t.Fatal("project's node removed from runtime registry")
	}
}

func TestRemoveNodeRefusesUnknownProjectOccupancy(t *testing.T) {
	admin := nodeAdminFixture(t)
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	admin.Projects = project.Open(book)
	if err := book.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(admin.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.RemoveNode(t.Context(), "node-test"); err == nil {
		t.Fatal("removed a node without knowing whether projects use it")
	}
	after, err := os.ReadFile(admin.Path)
	if err != nil || string(before) != string(after) {
		t.Fatal("failed occupancy check changed persistent configuration")
	}
	if len(admin.Nodes.Names()) != 1 {
		t.Fatal("failed occupancy check removed the runtime node")
	}
}

type recordingMembers struct {
	removed []string
	err     error
}

func (m *recordingMembers) RemoveMember(_ context.Context, nodeID string) error {
	m.removed = append(m.removed, nodeID)
	return m.err
}

// A machine that joined the cluster leaves it when it is removed from the
// page: its membership and the session this node keeps to it go with its
// configuration, so nothing keeps dialing it. When the cluster refuses,
// the configuration is kept, so the machine stays visible to try again.
func TestRemoveNodeLeavesTheClusterWithTheConfiguration(t *testing.T) {
	admin := nodeAdminFixture(t)
	members := &recordingMembers{err: errors.New("transfer coordination before removing this node")}
	admin.Members = members
	if err := admin.RemoveNode(t.Context(), "node-test"); err == nil || !strings.Contains(err.Error(), "transfer coordination") {
		t.Fatalf("a refused membership removal did not stop the removal: %v", err)
	}
	if _, kept := admin.Cfg.Nodes["node-test"]; !kept {
		t.Fatal("the configuration was dropped although the machine is still a member")
	}
	members.err = nil
	if err := admin.RemoveNode(t.Context(), "node-test"); err != nil {
		t.Fatal(err)
	}
	if _, kept := admin.Cfg.Nodes["node-test"]; kept || len(members.removed) != 2 || members.removed[1] != "node-test" {
		t.Fatalf("the machine did not leave the cluster with its configuration: kept=%v removed=%v", kept, members.removed)
	}
}

// When the machine has left the cluster but writing the configuration
// fails, the machine stays on the page and the next attempt finishes the
// job: leaving is asked again (the cluster treats a non-member as cleanup
// only) and the configuration goes this time.
func TestRemoveNodeRetriesAfterAFailedConfigurationWrite(t *testing.T) {
	admin := nodeAdminFixture(t)
	members := &recordingMembers{}
	admin.Members = members
	writes := 0
	admin.WriteConfigContext = func(ctx context.Context, path string, cfg *config.Config) error {
		writes++
		if writes == 1 {
			return errors.New("disk full")
		}
		return config.Save(path, cfg)
	}
	if err := admin.RemoveNode(t.Context(), "node-test"); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("a failed configuration write was not reported: %v", err)
	}
	if _, kept := admin.Cfg.Nodes["node-test"]; !kept {
		t.Fatal("the configuration was dropped although it was never written")
	}
	if err := admin.RemoveNode(t.Context(), "node-test"); err != nil {
		t.Fatal(err)
	}
	if _, kept := admin.Cfg.Nodes["node-test"]; kept || len(members.removed) != 2 {
		t.Fatalf("the retry did not finish the removal: kept=%v removed=%v", kept, members.removed)
	}
}
