//go:build linux || darwin

package acphost

import (
	"testing"

	"github.com/gopact-ai/acp"
)

func TestHostTerminalFrozenSharedReferenceAndOmittedContentRemainPromised(t *testing.T) {
	h, ch, _, _, col := terminalHostFixture(t, "auto")
	old := terminalCreate(t, ch, `printf original`, nil)
	terminalAwait(t, ch, old)
	terminalReferenceUpdate(h, col, "first-tool", old)
	terminalReferenceUpdate(h, col, "second-tool", old)
	if _, err := ch.ReleaseTerminal(t.Context(), &acp.ReleaseTerminalRequest{SessionID: "session", TerminalID: old}); err != nil {
		t.Fatal(err)
	}
	next := terminalCreate(t, ch, `printf replacement`, nil)
	terminalAwait(t, ch, next)
	terminalReferenceUpdate(h, col, "first-tool", next)
	if _, err := ch.ReleaseTerminal(t.Context(), &acp.ReleaseTerminalRequest{SessionID: "session", TerminalID: next}); err != nil {
		t.Fatal(err)
	}
	completed := acp.ToolCallStatusCompleted
	// Content is absent, not a supplied empty array. Completion must not
	// withdraw this other tool's already-promised frozen attachment.
	update := acp.SessionUpdate{SessionUpdate: acp.SessionUpdateTypeToolCallUpdate, ToolCallID: "second-tool", Status: &completed}
	col.handle(update)
	h.terminalUpdate(col, update)
	col.mu.Lock()
	originalOutput := col.tools[col.toolIndex["second-tool"]].Output
	nextOutput := col.tools[col.toolIndex["first-tool"]].Output
	count := len(col.terminalReleased)
	col.mu.Unlock()
	if originalOutput != "original" || nextOutput != "replacement" || count != 2 {
		t.Fatalf("shared or absent-content update lost promised bytes: old=%q next=%q count=%d", originalOutput, nextOutput, count)
	}
	terminalReferenceUpdate(h, col, "first-tool")
	col.mu.Lock()
	_, kept := col.terminalTexts[string(old)]
	_, discarded := col.terminalTexts[string(next)]
	oldRefs := append([]string(nil), col.terminalTools["second-tool"]...)
	col.mu.Unlock()
	h.mu.Lock()
	bytes := h.terminalOutputBytes[col]
	h.mu.Unlock()
	if !kept || discarded || len(oldRefs) != 1 || oldRefs[0] != string(old) || bytes != len("original") {
		t.Fatalf("explicit replacement reclaimed a shared ID or leaked credit: kept=%v discarded=%v refs=%v bytes=%d", kept, discarded, oldRefs, bytes)
	}
	terminalReferenceUpdate(h, col, "second-tool")
	col.mu.Lock()
	texts, released := len(col.terminalTexts), len(col.terminalReleased)
	col.mu.Unlock()
	h.mu.Lock()
	bytes = h.terminalOutputBytes[col]
	h.mu.Unlock()
	if texts != 0 || released != 0 || bytes != 0 {
		t.Fatalf("last explicit reference removal leaked ownership: texts=%d released=%d bytes=%d", texts, released, bytes)
	}
}
