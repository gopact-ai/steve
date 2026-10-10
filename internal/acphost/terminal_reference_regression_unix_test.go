//go:build linux || darwin

package acphost

import (
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/view"
)

func terminalReferenceUpdate(h *Host, col *collector, tool string, ids ...acp.TerminalID) {
	content := make([]acp.ToolCallContent, 0, len(ids))
	for _, id := range ids {
		content = append(content, acp.TerminalToolCallContent(id))
	}
	title := "terminal reference"
	update := acp.SessionUpdate{SessionUpdate: acp.SessionUpdateTypeToolCallUpdate, ToolCallID: acp.ToolCallID(tool), Title: &title, Content: content}
	col.handle(update)
	h.terminalUpdate(col, update)
}

func TestHostTerminalFrozenReferenceReplacementReusesCountAndBytes(t *testing.T) {
	for _, zero := range []bool{true, false} {
		name := "text"
		if zero {
			name = "zero"
		}
		t.Run(name, func(t *testing.T) {
			h, ch, _, _, col := terminalHostFixture(t, "auto")
			limit := uint64(16)
			if zero {
				limit = 0
			}
			var previous acp.TerminalID
			for i := 0; i < 40; i++ {
				id := terminalCreate(t, ch, `printf retained`, &limit)
				terminalAwait(t, ch, id)
				terminalReferenceUpdate(h, col, "one-tool", id)
				if _, err := ch.ReleaseTerminal(t.Context(), &acp.ReleaseTerminalRequest{SessionID: "session", TerminalID: id}); err != nil {
					t.Fatalf("cycle %d: %v", i, err)
				}
				col.mu.Lock()
				texts, released := len(col.terminalTexts), len(col.terminalReleased)
				_, oldText := col.terminalTexts[string(previous)]
				_, oldReleased := col.terminalReleased[string(previous)]
				output := col.tools[col.toolIndex["one-tool"]].Output
				col.mu.Unlock()
				if texts != 1 || released != 1 || oldText || oldReleased {
					t.Fatalf("cycle %d leaked unused frozen IDs: texts=%d released=%d oldText=%v oldReleased=%v", i, texts, released, oldText, oldReleased)
				}
				want := "retained"
				if zero {
					want = ""
				}
				if output != want {
					t.Fatalf("cycle %d lost current promised content: %q", i, output)
				}
				h.mu.Lock()
				bytes := h.terminalOutputBytes[col]
				active := len(h.terminals)
				h.mu.Unlock()
				if bytes != len(want) || active != 0 {
					t.Fatalf("cycle %d failed to reclaim replaced reference: bytes=%d active=%d", i, bytes, active)
				}
				previous = id
			}
		})
	}
}

func TestHostTerminalFrozenReferenceCountRefusesNewCapacityWithoutLosingRefs(t *testing.T) {
	h, ch, owner, _, col := terminalHostFixture(t, "auto")
	limit := uint64(0)
	ids := make([]acp.TerminalID, 0, maxTerminals)
	for i := 0; i < maxTerminals; i++ {
		id := terminalCreate(t, ch, `printf zero-capacity`, &limit)
		terminalAwait(t, ch, id)
		// Keep each promised attachment instead of replacing the previous one.
		tool := strings.Repeat("x", i+1)
		terminalReferenceUpdate(h, col, tool, id)
		if _, err := ch.ReleaseTerminal(t.Context(), &acp.ReleaseTerminalRequest{SessionID: "session", TerminalID: id}); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	owner.mu.Lock()
	before := len(owner.stages)
	owner.mu.Unlock()
	response, err := ch.CreateTerminal(t.Context(), &acp.CreateTerminalRequest{SessionID: "session", Command: "/bin/true", OutputByteLimit: &limit})
	if err == nil {
		t.Fatalf("33rd collector resource was admitted despite retained frozen IDs: %+v", response)
	}
	owner.mu.Lock()
	after := len(owner.stages)
	owner.mu.Unlock()
	if before != after {
		t.Fatal("capacity refusal reserved another durable native slot")
	}
	col.mu.Lock()
	if len(col.terminalTexts) != maxTerminals || len(col.terminalReleased) != maxTerminals {
		t.Error("capacity rejection discarded promised frozen IDs")
	}
	for i, id := range ids {
		tool := strings.Repeat("x", i+1)
		if !col.terminalReleased[string(id)] || len(col.terminalTools[tool]) != 1 || col.terminalTools[tool][0] != string(id) {
			t.Errorf("capacity rejection changed retained attachment %d", i)
		}
	}
	col.mu.Unlock()
	// An explicit content replacement releases capacity. Unattached create /
	// release remains reusable and does not discard the other 31 frozen refs.
	terminalReferenceUpdate(h, col, "x")
	next := terminalCreate(t, ch, `printf reusable`, &limit)
	terminalAwait(t, ch, next)
	if _, err := ch.ReleaseTerminal(t.Context(), &acp.ReleaseTerminalRequest{SessionID: "session", TerminalID: next}); err != nil {
		t.Fatal(err)
	}
	col.mu.Lock()
	defer col.mu.Unlock()
	if len(col.terminalTexts) != maxTerminals-1 || len(col.terminalReleased) != maxTerminals-1 {
		t.Fatal("unattached release changed frozen capacity")
	}
}

func TestHostTerminalAttachmentRegisteredBeforeProgressReleaseBarrier(t *testing.T) {
	h, ch, _, _, col := terminalHostFixture(t, "auto")
	id := terminalCreate(t, ch, `printf barrier-frozen`, nil)
	terminalAwait(t, ch, id)
	toAgent, clientOutput := io.Pipe()
	clientInput, agentOutput := io.Pipe()
	clientConn, err := acp.NewClient(clientInput, clientOutput, func(*acp.AgentCaller) acp.ClientHandler { return ch })
	if err != nil {
		t.Fatal(err)
	}
	var agentClient *acp.ClientCaller
	agentConn, err := acp.NewAgent(toAgent, agentOutput, func(client *acp.ClientCaller) acp.AgentHandler {
		agentClient = client
		return &lifecycleParticipant{version: acp.ProtocolVersionV1}
	})
	if err != nil {
		clientConn.Close()
		t.Fatal(err)
	}
	arrived, resume, settled := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var paused, finished atomic.Bool
	col.mu.Lock()
	col.progress = func(progress view.Progress) {
		if len(progress.Tools) > 0 && paused.CompareAndSwap(false, true) {
			close(arrived)
			<-resume
		}
		if progress.Answer == "notification-settled" && finished.CompareAndSwap(false, true) {
			close(settled)
		}
	}
	col.mu.Unlock()
	t.Cleanup(func() {
		select {
		case <-resume:
		default:
			close(resume)
		}
		clientConn.Close()
		agentConn.Close()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	title := "barrier-tool"
	if err := agentClient.Update(ctx, &acp.SessionNotification{SessionID: "session", Update: acp.SessionUpdate{SessionUpdate: acp.SessionUpdateTypeToolCall, ToolCallID: "barrier-tool", Title: &title, Content: []acp.ToolCallContent{acp.TerminalToolCallContent(id)}}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-arrived:
	case <-ctx.Done():
		t.Fatal("actual clientHandler.Update did not reach progress barrier")
	}
	released := make(chan error, 1)
	go func() {
		_, err := agentClient.ReleaseTerminal(ctx, &acp.ReleaseTerminalRequest{SessionID: "session", TerminalID: id})
		released <- err
	}()
	select {
	case err := <-released:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("release did not linearize while the progress callback paused")
	}
	close(resume)
	if err := agentClient.Update(ctx, &acp.SessionNotification{SessionID: "session", Update: acp.AgentMessageChunkSessionUpdate(acp.TextContentBlock("notification-settled"))}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-settled:
	case <-ctx.Done():
		t.Fatal("serial notification loop did not finish original attachment")
	}
	col.mu.Lock()
	output := col.tools[col.toolIndex["barrier-tool"]].Output
	frozen := col.terminalTexts[string(id)]
	retained := col.terminalReleased[string(id)]
	col.mu.Unlock()
	if output != "barrier-frozen" || frozen != "barrier-frozen" || !retained {
		t.Fatalf("release lost arrived attachment at progress barrier: output=%q frozen=%q retained=%v", output, frozen, retained)
	}
	h.mu.Lock()
	active := len(h.terminals)
	h.mu.Unlock()
	if active != 0 {
		t.Fatal("successful release did not invalidate live ID")
	}
}

func TestHostTerminalRenderSeparatorIncludedInAggregateBound(t *testing.T) {
	half := maxTerminalOutputBytes / 2
	first := strings.Repeat("a", half-len("中")) + "中"
	second := strings.Repeat("b", half-len("文")) + "文"
	col := &collector{
		tools: []view.Tool{{ID: "tool"}}, toolIndex: map[string]int{"tool": 0},
		terminalTools:    map[string][]string{"tool": {"first", "second"}},
		terminalToolBase: map[string]string{"tool": ""},
		terminalTexts:    map[string]string{"first": first, "second": second},
	}
	col.mu.Lock()
	col.renderTerminalToolsLocked()
	output := col.tools[0].Output
	col.mu.Unlock()
	if len(output) > maxTerminalOutputBytes || !utf8.ValidString(output) {
		t.Fatalf("rendered terminal fragments including separators exceeded byte bound: bytes=%d utf8=%v", len(output), utf8.ValidString(output))
	}
	if !strings.HasPrefix(output, first+"\n") || !strings.HasSuffix(output, "文") {
		t.Fatal("render clipping lost first fragment, separator or valid UTF-8 tail")
	}
}
