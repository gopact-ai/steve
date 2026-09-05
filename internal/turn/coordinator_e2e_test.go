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
		"codex":  {Harness: "codex", Default: true},
		"claude": {Harness: "claude"},
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
	coordinator := newCoordinator(t, catalog, store, capability.NewAssembler(nil), manager, 30*time.Second)
	result, err := handle(coordinator, context.Background(), "hello")
	if err != nil || result.Text != "echo: hello" {
		t.Fatalf("codex turn = %#v, %v", result, err)
	}
	result, err = handle(coordinator, context.Background(), "@claude perm check")
	if err != nil || result.Text != "[permission: selected/reject] echo: perm check" {
		t.Fatalf("claude turn = %#v, %v", result, err)
	}
	manager.Stop()

	manager, err = harness.NewManager(configs)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Stop)
	coordinator = restartCoordinator(t, coordinator, catalog, store, capability.NewAssembler(nil), manager, 30*time.Second)
	result, err = handle(coordinator, context.Background(), "after restart")
	if err != nil || result.Text != "echo: after restart" {
		t.Fatalf("resumed turn = %#v, %v", result, err)
	}
}

func TestPromptUsageIsRecordedOnAttemptsE2E(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "mockagent")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent")
	cmd.Dir = "../.."
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build mockagent: %v\n%s", err, output)
	}
	catalog, err := agent.NewCatalog(map[string]agent.Config{"codex": {Harness: "codex", Default: true}})
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	manager, err := harness.NewManager(map[string]harness.Config{"codex": {Command: bin, Permission: "deny"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Stop)
	coordinator := newCoordinator(t, catalog, store, capability.NewAssembler(nil), manager, 30*time.Second)
	for _, prompt := range []string{"reportusage", "reportusage cancelme", "legacy response"} {
		t.Run(prompt, func(t *testing.T) {
			_, turnErr := coordinator.Handle(t.Context(), Request{ConversationID: "chat", MessageID: prompt, Input: prompt})
			wantFailure := prompt == "reportusage cancelme"
			if (turnErr != nil) != wantFailure {
				t.Fatalf("turn error = %v, wantFailure %v", turnErr, wantFailure)
			}
			record, ok, err := coordinator.attempts.LatestForTurn(t.Context(), prompt)
			if err != nil || !ok || record.Usage == nil {
				t.Fatalf("attempt = %+v, found %v, error %v", record, ok, err)
			}
			u := record.Usage
			if prompt == "legacy response" {
				if u.Reported || u.Input != 0 || u.Output != 0 || u.CachedRead != 0 || u.CachedWrite != 0 {
					t.Fatalf("legacy attempt reused earlier usage: %+v", u)
				}
				return
			}
			if !u.Reported || u.Input != 100 || u.Output != 110 || u.CachedRead != 40 || u.CachedWrite != 50 || u.Context != 1600 {
				t.Fatalf("attempt lost prompt usage: %+v", u)
			}
		})
	}
}
