package turn

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/state"
)

func TestCoordinatorDynamicHarnessResumeE2E(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "mockagent")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent")
	cmd.Dir = "../.."
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build mockagent: %v\n%s", err, output)
	}
	catalog, err := agent.NewCatalog(map[string]agent.Config{
		"codex":  {Harness: "codex", Workspace: t.TempDir(), Default: true},
		"claude": {Harness: "claude", Workspace: t.TempDir()},
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	configs := map[string]harness.Config{
		"codex":  {Command: bin, Permission: "auto"},
		"claude": {Command: bin, Permission: "deny"},
	}
	manager, err := harness.NewManager(configs)
	if err != nil {
		t.Fatal(err)
	}
	coordinator := New(catalog, store, capability.NewAssembler(nil), manager, 30*time.Second)
	result, err := coordinator.Handle(context.Background(), "chat", "hello")
	if err != nil || result.Text != "echo: hello" {
		t.Fatalf("codex turn = %#v, %v", result, err)
	}
	result, err = coordinator.Handle(context.Background(), "chat", "@claude perm check")
	if err != nil || result.Text != "[permission: selected/reject] echo: perm check" {
		t.Fatalf("claude turn = %#v, %v", result, err)
	}
	manager.Stop()

	manager, err = harness.NewManager(configs)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Stop)
	coordinator = New(catalog, store, capability.NewAssembler(nil), manager, 30*time.Second)
	result, err = coordinator.Handle(context.Background(), "chat", "after restart")
	if err != nil || result.Text != "echo: after restart" {
		t.Fatalf("resumed turn = %#v, %v", result, err)
	}
}
