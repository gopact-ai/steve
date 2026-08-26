package acphost

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/view"
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

func TestPermissionAskCallsHook(t *testing.T) {
	h := newTestHost(t, "read")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sid, generation, err := h.OpenSession(ctx, "", SessionConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	asked := make(chan permission.Ask, 1)
	out, _, err := h.PromptTurn(ctx, sid, generation, "perm check", nil, func(_ context.Context, ask permission.Ask) (acp.RequestPermissionOutcome, error) {
		asked <- ask
		return permission.Choose(true, ask.Options), nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case ask := <-asked:
		if ask.ToolName != "dangerous operation" {
			t.Fatalf("ask = %#v", ask)
		}
	default:
		t.Fatal("ask hook was not called")
	}
	if !strings.Contains(out, "[permission: selected/allow]") {
		t.Fatalf("expected asked allow, got: %q", out)
	}
}

func TestCloseRejectsRestart(t *testing.T) {
	h := newTestHost(t, "auto")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, _, err := h.OpenSession(ctx, "", SessionConfig{Workdir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	h.Close()
	if _, _, err := h.OpenSession(ctx, "", SessionConfig{Workdir: t.TempDir()}); !errors.Is(err, ErrClosed) {
		t.Fatalf("open after close = %v, want ErrClosed", err)
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

func TestCollectorCapsOutput(t *testing.T) {
	col := &collector{}
	big := strings.Repeat("a", maxCollectBytes+1024)
	col.handle(acp.SessionUpdate{
		SessionUpdate: acp.SessionUpdateTypeAgentMessageChunk,
		Content:       acp.TextContentBlock(big),
	})
	out, _ := col.result()
	if len(out) > maxCollectBytes {
		t.Fatalf("collector output %d bytes exceeds cap %d", len(out), maxCollectBytes)
	}
	if !strings.Contains(out, "truncated") {
		t.Fatalf("collector output missing truncation marker")
	}
	// Subsequent chunks must be dropped after overflow.
	col.handle(acp.SessionUpdate{
		SessionUpdate: acp.SessionUpdateTypeAgentMessageChunk,
		Content:       acp.TextContentBlock("more"),
	})
	out, _ = col.result()
	if strings.Contains(out, "more") {
		t.Fatalf("collector kept chunks after overflow")
	}
}

func TestCollectorDoesNotSplitRune(t *testing.T) {
	col := &collector{}
	// Force a cut in the middle of a 3-byte rune sequence.
	col.handle(acp.SessionUpdate{
		SessionUpdate: acp.SessionUpdateTypeAgentMessageChunk,
		Content:       acp.TextContentBlock(strings.Repeat("a", maxCollectBytes-1) + "好"),
	})
	out, _ := col.result()
	if !utf8.ValidString(out) {
		t.Fatalf("collector output is invalid UTF-8")
	}
}

func TestCollectorTracksToolLifecycle(t *testing.T) {
	var got []view.Progress
	col := &collector{progress: func(p view.Progress) { got = append(got, p) }}
	id := acp.ToolCallID("tool-1")
	title := "read file"
	col.handle(acp.SessionUpdate{
		SessionUpdate: acp.SessionUpdateTypeToolCall,
		ToolCallID:    id,
		Title:         &title,
		RawInput:      map[string]string{"path": "README.md"},
	})
	done := acp.ToolCallStatusCompleted
	col.handle(acp.SessionUpdate{
		SessionUpdate: acp.SessionUpdateTypeToolCallUpdate,
		ToolCallID:    id,
		Status:        &done,
		RawOutput:     "# hi",
	})
	col.handle(acp.SessionUpdate{
		SessionUpdate: acp.SessionUpdateTypeUsageUpdate,
		Used:          1600,
		Size:          128000,
	})
	if len(got) != 3 {
		t.Fatalf("progress events = %d, want 3", len(got))
	}
	if len(got[0].Tools) != 1 || got[0].Tools[0].Status != view.ToolRunning || !strings.Contains(got[0].Tools[0].Input, "README.md") {
		t.Fatalf("start = %#v", got[0].Tools)
	}
	if got[1].Tools[0].Status != view.ToolCompleted || got[1].Tools[0].Output != "# hi" {
		t.Fatalf("done = %#v", got[1].Tools)
	}
	if got[2].Usage.ContextTokens != 1600 || got[2].Usage.ContextWindow != 128000 {
		t.Fatalf("usage = %#v", got[2].Usage)
	}
	_, activity := col.result()
	if len(activity) != 1 || !strings.Contains(activity[0], "read file") {
		t.Fatalf("activity = %v", activity)
	}
}

func TestCollectorTracksThoughtChunks(t *testing.T) {
	var got []view.Progress
	col := &collector{progress: func(p view.Progress) { got = append(got, p) }}
	col.handle(acp.SessionUpdate{
		SessionUpdate: acp.SessionUpdateTypeAgentThoughtChunk,
		Content:       acp.TextContentBlock("先看仓库"),
	})
	col.handle(acp.SessionUpdate{
		SessionUpdate: acp.SessionUpdateTypeAgentThoughtChunk,
		Content:       acp.TextContentBlock("再改卡片"),
	})
	if len(got) != 2 || got[1].Reasoning != "先看仓库再改卡片" {
		t.Fatalf("thought = %#v", got)
	}
}

func TestPromptBlocksOmitsImagesWithoutCapability(t *testing.T) {
	blocks := promptBlocks("hi", []Image{{MIME: "image/png", Data: []byte("x")}}, nil)
	if len(blocks) != 1 || !strings.Contains(blocks[0].Text, "images omitted") {
		t.Fatalf("blocks = %#v", blocks)
	}
}

func TestPromptBlocksIncludesImages(t *testing.T) {
	caps := &acp.AgentCapabilities{PromptCapabilities: &acp.PromptCapabilities{Image: true}}
	blocks := promptBlocks("hi", []Image{{MIME: "image/png", Data: []byte("x")}}, caps)
	if len(blocks) != 2 || blocks[1].Type != acp.ContentBlockTypeImage {
		t.Fatalf("blocks = %#v", blocks)
	}
}

func TestSplitToolContentReadsDiffAndText(t *testing.T) {
	old := "a"
	diff, text := splitToolContent([]acp.ToolCallContent{
		{Type: acp.ToolCallContentTypeDiff, Path: "/tmp/x.go", NewText: "b", OldText: &old},
		{Type: acp.ToolCallContentTypeContent, Content: acp.TextContentBlock("done")},
	})
	if diff != "/tmp/x.go\nb" {
		t.Fatalf("diff = %q", diff)
	}
	if text != "done" {
		t.Fatalf("text = %q", text)
	}
}

func TestToolCallContentFillsMissingIO(t *testing.T) {
	col := &collector{}
	id := acp.ToolCallID("tool-diff")
	title := "Write file"
	kind := acp.ToolKindEdit
	col.handle(acp.SessionUpdate{
		SessionUpdate: acp.SessionUpdateTypeToolCall,
		ToolCallID:    id,
		Title:         &title,
		Kind:          &kind,
		Content: []acp.ToolCallContent{
			{Type: acp.ToolCallContentTypeDiff, Path: "/tmp/x.go", NewText: "hello"},
		},
	})
	if len(col.tools) != 1 {
		t.Fatalf("tools = %#v", col.tools)
	}
	tool := col.tools[0]
	if tool.Kind != "edit" || tool.Name != "Write file" {
		t.Fatalf("tool = %#v", tool)
	}
	if !strings.Contains(tool.Input, "hello") || !strings.Contains(tool.Input, "/tmp/x.go") {
		t.Fatalf("diff should become the input: %#v", tool)
	}
}
