package turn

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
)

// TestTaskSurvivesAGatewayRestartE2E drives turns against the mockagent
// process and reads task records from SQLite, because the point of a task is
// that it is still there after the process that opened it is gone.
func TestTaskSurvivesAGatewayRestartE2E(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "mockagent")
	build := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent")
	build.Dir = "../.."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build mockagent: %v\n%s", err, output)
	}

	stateDir := t.TempDir()
	workspace := t.TempDir()
	nodeDir := t.TempDir()
	configs := map[string]harness.Config{"codex": {Command: bin, Permission: "auto"}}
	catalog, err := agent.NewCatalog(map[string]agent.Config{
		"codex": {Harness: "codex", Default: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Each boot reopens every owner on the same durable ledger.
	var book *ledger.Ledger
	boot := func() (*Coordinator, *harness.Manager, *task.Store) {
		var err error
		book, err = ledger.Open(stateDir, ledger.Options{})
		if err != nil {
			t.Fatal(err)
		}
		manager, err := harness.NewManager(configs)
		if err != nil {
			t.Fatal(err)
		}
		tasks, err := task.OpenLedger(book)
		if err != nil {
			t.Fatal(err)
		}
		store, err := state.OpenLedger(book)
		if err != nil {
			t.Fatal(err)
		}
		coordinator := newCoordinatorIn(t, map[string]string{"codex": workspace}, catalog, store, capability.NewAssembler(nil), manager, 30*time.Second, onLedger(book), withTasks(tasks, "e2e-node"))
		coordinator.SetArtifacts(artifact.New(filepath.Join(stateDir, "artifacts"), book, coordinator.projects, artifact.LocalNodes{Dir: nodeDir}))
		coordinator.artifacts.SetExecution(coordinator.executions)
		return coordinator, manager, tasks
	}

	coordinator, manager, _ := boot()
	if result, err := handle(coordinator, context.Background(), "wire the node link"); err != nil || result.Text != "echo: wire the node link" {
		t.Fatalf("first turn = %#v, %v", result, err)
	}
	if result, err := handle(coordinator, context.Background(), "add reconnect"); err != nil || result.Text != "echo: add reconnect" {
		t.Fatalf("second turn = %#v, %v", result, err)
	}
	manager.Stop()

	// Reload rather than inspecting the old owner's cache.
	onDisk, err := task.OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	if len(onDisk.List("")) != 1 {
		t.Fatalf("tasks on disk = %d; want one task across both turns", len(onDisk.List("")))
	}
	stored, found := onDisk.Get("1")
	if !found {
		t.Fatal("original task was not persisted")
	}
	if stored.Goal != "wire the node link" || stored.Node != "e2e-node" || stored.Member != "codex" {
		t.Fatalf("stored task = %+v", stored)
	}
	if stored.Budget.Turns != 2 || len(stored.Attempts) != 2 {
		t.Fatalf("turns=%d attempts=%d; want 2 and 2", stored.Budget.Turns, len(stored.Attempts))
	}
	if stored.Budget.Elapsed <= 0 {
		t.Fatalf("elapsed was never accumulated: %v", stored.Budget.Elapsed)
	}
	if err := book.Close(); err != nil {
		t.Fatal(err)
	}

	// Restart everything the way a gateway restart would.
	coordinator, manager, tasks := boot()
	t.Cleanup(manager.Stop)
	t.Cleanup(func() { book.Close() })
	if result, err := handle(coordinator, context.Background(), "after restart"); err != nil || result.Text != "echo: after restart" {
		t.Fatalf("resumed turn = %#v, %v", result, err)
	}
	all := tasks.List("chat")
	if len(all) != 1 {
		t.Fatalf("after restart tasks = %d; want the same task continued", len(all))
	}
	if all[0].ID != "1" || all[0].Budget.Turns != 3 {
		t.Fatalf("after restart = %+v; want task 1 on its third turn", all[0])
	}
	listed, err := handle(coordinator, context.Background(), "/tasks")
	if err != nil {
		t.Fatalf("/tasks: %v", err)
	}
	if listed.Text == "" || listed.Title == "" {
		t.Fatalf("/tasks produced nothing: %#v", listed)
	}
}
