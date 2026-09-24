package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/gopact-ai/steve/internal/config"
	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/roster"
)

func workerAdmissionFixture(t *testing.T) *Service {
	t.Helper()
	admin := agentAdminFixture(t)
	admin.Nodes = node.NewRegistry("hub-test", nil)
	t.Cleanup(admin.Nodes.Close)
	admin.Fleet = roster.New(admin.Catalog)
	return admin
}

func TestAdmitWorkerRecordsAndDialsANewWorker(t *testing.T) {
	admin := workerAdmissionFixture(t)
	server := startAgentAdminNode(t, nil)
	var dialed atomic.Bool
	dial := func(ctx context.Context, _ string) (net.Conn, error) {
		dialed.Store(true)
		var d net.Dialer
		return d.DialContext(ctx, "tcp", server.Addr())
	}
	err := admin.AdmitWorker(t.Context(), "node-test", node.Config{Addr: server.Addr(), Token: "test-node-token", DialContext: dial})
	if err != nil {
		t.Fatal(err)
	}
	want := config.Node{Addr: server.Addr(), Token: "test-node-token", Level: "internal"}
	if got := admin.cfg().Nodes["node-test"]; got != want {
		t.Fatalf("configured worker = %+v, want %+v", got, want)
	}
	raw, err := os.ReadFile(admin.Path)
	if err != nil {
		t.Fatal(err)
	}
	var saved config.Config
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if got := saved.Nodes["node-test"]; got != want {
		t.Fatalf("saved worker = %+v, want %+v", got, want)
	}
	if !slices.Contains(admin.Nodes.Names(), "node-test") {
		t.Fatalf("registry does not know the worker: %v", admin.Nodes.Names())
	}
	if !dialed.Load() {
		t.Fatal("the worker was not reached through the supplied dialer")
	}
}

func TestAdmitWorkerKeepsAnIdenticalRegistration(t *testing.T) {
	admin := workerAdmissionFixture(t)
	server := startAgentAdminNode(t, nil)
	admin.cfg().Nodes = map[string]config.Node{"node-test": {Addr: server.Addr(), Token: "test-node-token", Level: "restricted"}}
	writes := 0
	admin.WriteConfig = func(path string, cfg *config.Config) error {
		writes++
		return config.Save(path, cfg)
	}
	if err := admin.AdmitWorker(t.Context(), "node-test", node.Config{Addr: server.Addr(), Token: "test-node-token", Level: "public"}); err != nil {
		t.Fatal(err)
	}
	if writes != 0 {
		t.Fatalf("an identical registration rewrote the configuration %d times", writes)
	}
	if got := admin.cfg().Nodes["node-test"].Level; got != "restricted" {
		t.Fatalf("an identical registration changed the level to %q", got)
	}
	if !slices.Contains(admin.Nodes.Names(), "node-test") {
		t.Fatalf("registry does not know the worker: %v", admin.Nodes.Names())
	}
}

func TestAdmitWorkerRefusesADifferentRegistration(t *testing.T) {
	admin := workerAdmissionFixture(t)
	existing := config.Node{Addr: "127.0.0.1:1", Token: "old-token", Level: "internal"}
	admin.cfg().Nodes = map[string]config.Node{"node-test": existing}
	err := admin.AdmitWorker(t.Context(), "node-test", node.Config{Addr: "127.0.0.1:2", Token: "new-token"})
	if !errors.Is(err, coordination.ErrConflict) {
		t.Fatalf("a different registration was not a conflict: %v", err)
	}
	if got := admin.cfg().Nodes["node-test"]; got != existing {
		t.Fatalf("a refused registration changed the configuration: %+v", got)
	}
	if slices.Contains(admin.Nodes.Names(), "node-test") {
		t.Fatal("a refused registration reached the registry")
	}
}

func TestAdmitWorkerLeavesNothingBehindWhenTheConfigurationIsNotSaved(t *testing.T) {
	admin := workerAdmissionFixture(t)
	admin.WriteConfig = func(string, *config.Config) error { return errors.New("disk full") }
	err := admin.AdmitWorker(t.Context(), "node-test", node.Config{Addr: "127.0.0.1:1", Token: "test-node-token"})
	if err == nil {
		t.Fatal("a failed configuration write was not reported")
	}
	if _, ok := admin.cfg().Nodes["node-test"]; ok {
		t.Fatal("the unsaved worker stayed in the configuration")
	}
	if slices.Contains(admin.Nodes.Names(), "node-test") {
		t.Fatal("the unsaved worker reached the registry")
	}
}

// A worker saved without its directory synced stays recorded; when it
// cannot be reached either, both failures are reported.
func TestAdmitWorkerReportsAnUnsyncedSaveAlongWithAnUnreachableWorker(t *testing.T) {
	admin := workerAdmissionFixture(t)
	admin.WriteConfig = func(path string, cfg *config.Config) error {
		if err := config.Save(path, cfg); err != nil {
			return err
		}
		return &config.CommittedError{Err: errors.New("sync config directory")}
	}
	unreachable := errors.New("worker unreachable")
	dial := func(context.Context, string) (net.Conn, error) { return nil, unreachable }
	err := admin.AdmitWorker(t.Context(), "node-test", node.Config{Addr: "127.0.0.1:1", Token: "test-node-token", DialContext: dial})
	if !config.Committed(err) {
		t.Fatalf("the unsynced save was not reported: %v", err)
	}
	if !errors.Is(err, unreachable) {
		t.Fatalf("the unreachable worker was not reported: %v", err)
	}
	if _, ok := admin.cfg().Nodes["node-test"]; !ok {
		t.Fatal("the saved worker was not recorded")
	}
}
