package acphost

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/permission"
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

func TestOpenSessionRejectsUnsupportedMCPTransport(t *testing.T) {
	h := newTestHost(t, "deny")
	_, _, err := h.OpenSession(t.Context(), "", SessionConfig{
		Workdir: t.TempDir(),
		MCPServers: []acp.MCPServer{
			acp.HTTPMCPServer("remote", "https://example.com/mcp", nil),
		},
	})
	if err == nil || !strings.Contains(err.Error(), "HTTP MCP") {
		t.Fatalf("expected unsupported HTTP MCP error, got %v", err)
	}
}

func TestPromptRejectsConcurrentSessionTurn(t *testing.T) {
	h := newTestHost(t, "deny")
	sid, generation, err := h.OpenSession(t.Context(), "", SessionConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.active[sid] = h.generation
	h.mu.Unlock()
	if _, _, err := h.Prompt(t.Context(), sid, generation, "hello", nil); err == nil {
		t.Fatal("expected concurrent prompt error")
	}
}

func newTestHost(t *testing.T, policy string) *Host {
	t.Helper()
	broker, err := permission.New(policy)
	if err != nil {
		t.Fatal(err)
	}
	h := New(Config{
		Command:    buildMockAgent(t),
		ProcessDir: t.TempDir(),
		Permission: broker,
	})
	t.Cleanup(h.Stop)
	return h
}

func TestPromptRoundTrip(t *testing.T) {
	h := newTestHost(t, "auto")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sid, generation, err := h.OpenSession(ctx, "", SessionConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewChatSession: %v", err)
	}
	out, _, err := h.Prompt(ctx, sid, generation, "hello world", nil)
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if out != "echo: hello world" {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestPromptCanceledStopReason(t *testing.T) {
	h := newTestHost(t, "deny")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sid, generation, err := h.OpenSession(ctx, "", SessionConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := h.Prompt(ctx, sid, generation, "cancelme now", nil)
	if err == nil {
		t.Fatal("expected canceled turn error")
	}
	if !errors.Is(err, ErrTurnCanceled) || !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "echo: cancelme now" {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestPermissionAutoAllow(t *testing.T) {
	h := newTestHost(t, "auto")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sid, generation, err := h.OpenSession(ctx, "", SessionConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewChatSession: %v", err)
	}
	out, _, err := h.Prompt(ctx, sid, generation, "perm check", nil)
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

	sid, generation, err := h.OpenSession(ctx, "", SessionConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewChatSession: %v", err)
	}
	out, _, err := h.Prompt(ctx, sid, generation, "perm check", nil)
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

	sid, generation, err := h.OpenSession(ctx, "", SessionConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewChatSession: %v", err)
	}
	if _, _, err := h.Prompt(ctx, sid, generation, "first", nil); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	h.Stop()

	// A new session on the same host must transparently restart the process.
	sid2, generation2, err := h.OpenSession(ctx, "", SessionConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewChatSession after stop: %v", err)
	}
	out, _, err := h.Prompt(ctx, sid2, generation2, "second", nil)
	if err != nil {
		t.Fatalf("Prompt after restart: %v", err)
	}
	if out != "echo: second" {
		t.Fatalf("unexpected output after restart: %q", out)
	}
	if _, _, err := h.Prompt(ctx, sid, generation, "stale", nil); err == nil {
		t.Fatal("old session runner was accepted by a new process generation")
	}
}

func TestResumeAfterProcessRestart(t *testing.T) {
	h := newTestHost(t, "auto")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	workdir := t.TempDir()
	sid, _, err := h.OpenSession(ctx, "", SessionConfig{Workdir: workdir})
	if err != nil {
		t.Fatal(err)
	}
	h.Stop()
	resumed, _, err := h.OpenSession(ctx, sid, SessionConfig{Workdir: workdir})
	if err != nil {
		t.Fatal(err)
	}
	if resumed != sid {
		t.Fatalf("resumed session = %q, want %q", resumed, sid)
	}
}
