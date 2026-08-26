package turn

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
)

// TestTaskSurvivesAGatewayRestartE2E drives real turns against a real agent
// process and asserts the task file on disk, because the point of a task is
// that it is still there after the process that opened it is gone.
func TestTaskSurvivesAGatewayRestartE2E(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "mockagent")
	build := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent")
	build.Dir = "../.."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build mockagent: %v\n%s", err, output)
	}

	stateDir := t.TempDir()
	taskPath := filepath.Join(stateDir, "tasks.json")
	configs := map[string]harness.Config{"codex": {Command: bin, Permission: "auto"}}
	catalog, err := agent.NewCatalog(map[string]agent.Config{
		"codex": {Harness: "codex", Workspace: t.TempDir(), Default: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(filepath.Join(stateDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	boot := func() (*Coordinator, *harness.Manager, *task.Store) {
		manager, err := harness.NewManager(configs)
		if err != nil {
			t.Fatal(err)
		}
		tasks, err := task.Open(taskPath)
		if err != nil {
			t.Fatal(err)
		}
		coordinator := New(catalog, store, capability.NewAssembler(nil), manager, 30*time.Second)
		coordinator.SetTasks(tasks, "e2e-node")
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

	// Read the file rather than the store: durability is the claim under test.
	raw, err := os.ReadFile(taskPath)
	if err != nil {
		t.Fatalf("tasks file was never written: %v", err)
	}
	var onDisk struct {
		NextID int                  `json:"next_id"`
		Tasks  map[string]task.Task `json:"tasks"`
	}
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("tasks file is not readable JSON: %v\n%s", err, raw)
	}
	if len(onDisk.Tasks) != 1 {
		t.Fatalf("tasks on disk = %d; want one task across both turns", len(onDisk.Tasks))
	}
	stored := onDisk.Tasks["1"]
	if stored.Goal != "wire the node link" || stored.Node != "e2e-node" || stored.Member != "codex" {
		t.Fatalf("stored task = %+v", stored)
	}
	if stored.Budget.Turns != 2 || len(stored.Attempts) != 2 {
		t.Fatalf("turns=%d attempts=%d; want 2 and 2", stored.Budget.Turns, len(stored.Attempts))
	}
	if stored.Budget.Elapsed <= 0 {
		t.Fatalf("elapsed was never accumulated: %v", stored.Budget.Elapsed)
	}

	// Restart everything the way a gateway restart would.
	coordinator, manager, tasks := boot()
	t.Cleanup(manager.Stop)
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
