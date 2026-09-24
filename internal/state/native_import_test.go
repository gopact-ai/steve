package state

import (
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/nativehistory"
)

func TestNativeImportRetryPreservesUsedConversationAndProvenance(t *testing.T) {
	book := testLedger(t)
	s, err := OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	ref := nativehistory.Reference{ID: "import_" + strings.Repeat("a", 64), Harness: "codex", NativeID: "native", SourceHome: "/original", SourceWorkdir: "/work", Revision: "rev", Digest: "digest", ImportedAt: time.Now().UTC()}
	initial := Session{ConversationID: "console:import", AgentID: "agent", HarnessID: "codex", NodeID: "worker", ProjectID: "p", ProjectVersion: 1, Workspace: "/work", NativeImport: ref.Clone()}
	if err := s.InstallNativeSession(initial); err != nil {
		t.Fatal(err)
	}
	used := s.Conversation(initial.ConversationID).Sessions[initial.AgentID]
	used.UpstreamID = "ns_managed"
	if err := s.SaveSession(used); err != nil {
		t.Fatal(err)
	}
	initial.NativeImport.NativeID = "mutated"
	if got := s.Conversation(initial.ConversationID).Sessions[initial.AgentID]; got.NativeImport.NativeID != ref.NativeID {
		t.Fatal("caller mutated stored provenance")
	}
	initial.NativeImport = ref.Clone()
	restored, err := OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.InstallNativeSession(initial); err != nil {
		t.Fatal(err)
	}
	if got := restored.Conversation(initial.ConversationID); got.ActiveAgent != initial.AgentID || got.Sessions[initial.AgentID].UpstreamID != "ns_managed" {
		t.Fatal("retry reset used conversation")
	}
	initial.NodeID = "other"
	if err := restored.InstallNativeSession(initial); err == nil {
		t.Fatal("changed destination accepted")
	}
}

// testLedger opens a ledger that lives as long as the test.
func testLedger(t *testing.T) *ledger.Ledger {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = book.Close() })
	return book
}
