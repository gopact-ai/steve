package state

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestProjectTransferNeverCarriesPendingCredentials(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	retained := Session{ConversationID: "chat", AgentID: "agent", ProjectID: "p", AgentToken: "old", PendingAgentToken: "pending", UpstreamID: "ns_context"}
	if err := s.SaveSession(retained); err != nil {
		t.Fatal(err)
	}
	if err := s.ArchiveSession("chat", "agent", "now"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSession(retained); err != nil {
		t.Fatal(err)
	}
	out, err := ExportProject(s.doc, "p", []string{"chat"})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []Session{out.Conversations["chat"].Sessions["agent"], out.Conversations["chat"].Archived[0].Session} {
		if v.AgentToken != "" || v.PendingAgentToken != "" || v.UpstreamID != "" {
			t.Fatal("transfer exposed native credentials")
		}
	}
	for _, archived := range []bool{false, true} {
		raw, _ := json.Marshal(out)
		var imported ProjectTransfer
		if err := json.Unmarshal(raw, &imported); err != nil {
			t.Fatal(err)
		}
		c := imported.Conversations["chat"]
		if archived {
			c.Archived[0].PendingAgentToken = "injected"
		} else {
			v := c.Sessions["agent"]
			v.PendingAgentToken = "injected"
			c.Sessions["agent"] = v
		}
		imported.Conversations["chat"] = c
		if err := ImportProject(s.doc, imported); err == nil {
			t.Fatal("import accepted pending credentials")
		}
	}
}
