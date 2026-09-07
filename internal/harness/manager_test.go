package harness

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/acphost"
)

func TestManagerAllowsEmptyConfigurationBeforeFirstRegistration(t *testing.T) {
	manager, err := NewManager(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Stop)
	if _, err := manager.host(Placement{Harness: "codex"}); err == nil || !strings.Contains(err.Error(), "unknown harness") {
		t.Fatalf("missing harness did not produce a configuration error: %v", err)
	}
	if err := manager.Set("codex", Config{Command: "codex-acp", Permission: "read"}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.host(Placement{Harness: "codex"}); err != nil {
		t.Fatalf("first registration failed: %v", err)
	}
}

func TestManagerPublishesPreparedConfigurationWithoutReplacingHosts(t *testing.T) {
	live, err := NewManager(map[string]Config{"one": {Command: "original", Permission: "read"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(live.Stop)
	host, err := live.host(Placement{Harness: "one"})
	if err != nil {
		t.Fatal(err)
	}
	configs := map[string]Config{"one": {Command: "updated", Permission: "read"}, "two": {Command: "second", Args: []string{"acp"}, Permission: "read"}}
	prepared, err := NewManager(configs)
	if err != nil {
		t.Fatal(err)
	}
	configs["two"].Args[0] = "changed"
	live.Publish(prepared)
	if got, err := live.host(Placement{Harness: "one"}); err != nil || got != host {
		t.Fatal("publishing stopped an existing host")
	}
	if live.configs["two"].Args[0] != "acp" || live.configs["one"].Command != "updated" {
		t.Fatal("published configs differ from the validated candidate")
	}
	if err := prepared.Set("two", Config{Command: "later", Permission: "read"}); err != nil {
		t.Fatal(err)
	}
	if live.configs["two"].Command != "second" {
		t.Fatal("candidate mutation changed the published configs")
	}
}

func TestManagerStartsHarnessesLazily(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "mockagent")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent")
	cmd.Dir = "../.."
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build mockagent: %v\n%s", err, output)
	}
	manager, err := NewManager(map[string]Config{
		"one": {Command: bin, Permission: "auto"},
		"two": {Command: bin, Permission: "deny"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Stop)
	if len(manager.hosts) != 0 {
		t.Fatal("manager started hosts eagerly")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := manager.OpenSession(ctx, Placement{Harness: "one"}, "", t.TempDir(), nil); err != nil {
		t.Fatal(err)
	}
	if len(manager.hosts) != 1 {
		t.Fatalf("started hosts = %d, want 1", len(manager.hosts))
	}
	if _, err := manager.OpenSession(ctx, Placement{Harness: "two"}, "", t.TempDir(), nil); err != nil {
		t.Fatal(err)
	}
	if len(manager.hosts) != 2 {
		t.Fatalf("started hosts = %d, want 2", len(manager.hosts))
	}
}

func TestManagerStopRejectsNewSessionAndHostRestart(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "mockagent")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent")
	cmd.Dir = "../.."
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build mockagent: %v\n%s", err, output)
	}
	manager, err := NewManager(map[string]Config{"one": {Command: bin, Permission: "deny"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	runner, err := manager.OpenSession(ctx, Placement{Harness: "one"}, "", t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	manager.Stop()
	if _, err := manager.OpenSession(ctx, Placement{Harness: "one"}, "", t.TempDir(), nil); err == nil {
		t.Fatal("open after stop succeeded")
	}
	if _, _, err := runner.Prompt(ctx, "after stop", nil); !errors.Is(err, acphost.ErrClosed) {
		t.Fatalf("prompt after manager stop = %v, want ErrClosed", err)
	}
}

func TestManagerRestartClosesHostsAndAllowsNewSession(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "mockagent")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent")
	cmd.Dir = "../.."
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build mockagent: %v\n%s", err, output)
	}
	manager, err := NewManager(map[string]Config{"one": {Command: bin, Permission: "deny"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Stop)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	runner, err := manager.OpenSession(ctx, Placement{Harness: "one"}, "", t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Restart(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runner.Prompt(ctx, "after restart", nil); !errors.Is(err, acphost.ErrClosed) {
		t.Fatalf("old runner after restart = %v, want ErrClosed", err)
	}
	if _, err := manager.OpenSession(ctx, Placement{Harness: "one"}, "", t.TempDir(), nil); err != nil {
		t.Fatal(err)
	}
	if len(manager.hosts) != 1 {
		t.Fatalf("hosts after restart = %d", len(manager.hosts))
	}
}

func TestManagerCreatesWorkspace(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "mockagent")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent")
	cmd.Dir = "../.."
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build mockagent: %v\n%s", err, output)
	}
	manager, err := NewManager(map[string]Config{"one": {Command: bin, Permission: "deny"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Stop)
	workspace := filepath.Join(t.TempDir(), "missing", "workspace")
	if _, err := manager.OpenSession(t.Context(), Placement{Harness: "one"}, "", workspace, nil); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(workspace); err != nil || !info.IsDir() {
		t.Fatalf("workspace was not created: %v", err)
	}
}
