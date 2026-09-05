package harness

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/acphost"
)

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
