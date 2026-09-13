package state

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/nativehistory"
)

func TestNativeImportRetryPreservesUsedConversationAndProvenance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := Open(path)
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
	restored, err := Open(path)
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
