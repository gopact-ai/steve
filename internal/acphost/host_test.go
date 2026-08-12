package acphost

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// buildMockAgent compiles cmd/mockagent into a temp dir and returns its path.
func buildMockAgent(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "mockagent")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/gopact-ai/steve/cmd/mockagent")
	cmd.Dir = "../.."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build mockagent: %v\n%s", err, out)
	}
	return bin
}

func newTestHost(t *testing.T, permission string) *Host {
	t.Helper()
	h := New(Config{
		Command:    buildMockAgent(t),
		Workdir:    t.TempDir(),
		Permission: permission,
	})
	t.Cleanup(h.Stop)
	return h
}

func TestPromptRoundTrip(t *testing.T) {
	h := newTestHost(t, "auto")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sid, err := h.NewChatSession(ctx)
	if err != nil {
		t.Fatalf("NewChatSession: %v", err)
	}
	out, _, err := h.Prompt(ctx, sid, "hello world", nil)
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if out != "echo: hello world" {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestPermissionAutoAllow(t *testing.T) {
	h := newTestHost(t, "auto")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sid, err := h.NewChatSession(ctx)
	if err != nil {
		t.Fatalf("NewChatSession: %v", err)
	}
	out, _, err := h.Prompt(ctx, sid, "perm check", nil)
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if !strings.Contains(out, "[permission: selected/allow]") {
		t.Fatalf("expected auto-allow outcome, got: %q", out)
	}
}

func TestPermissionDeny(t *testing.T) {
	h := newTestHost(t, "deny")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sid, err := h.NewChatSession(ctx)
	if err != nil {
		t.Fatalf("NewChatSession: %v", err)
	}
	out, _, err := h.Prompt(ctx, sid, "perm check", nil)
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if !strings.Contains(out, "[permission: selected/reject]") {
		t.Fatalf("expected deny outcome, got: %q", out)
	}
}

func TestRestartAfterProcessExit(t *testing.T) {
	h := newTestHost(t, "auto")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sid, err := h.NewChatSession(ctx)
	if err != nil {
		t.Fatalf("NewChatSession: %v", err)
	}
	if _, _, err := h.Prompt(ctx, sid, "first", nil); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	h.Stop()

	// A new session on the same host must transparently restart the process.
	sid2, err := h.NewChatSession(ctx)
	if err != nil {
		t.Fatalf("NewChatSession after stop: %v", err)
	}
	out, _, err := h.Prompt(ctx, sid2, "second", nil)
	if err != nil {
		t.Fatalf("Prompt after restart: %v", err)
	}
	if out != "echo: second" {
		t.Fatalf("unexpected output after restart: %q", out)
	}
}
