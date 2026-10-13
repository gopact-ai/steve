package state

import (
	"errors"
	"testing"
)

func TestPendingCloseCannotBeOverwrittenRestoredOrTransferred(t *testing.T) {
	book := testLedger(t)
	s, err := OpenLedger(book)
	if err != nil {
		t.Fatal(err)
	}
	original := Session{ConversationID: "chat", AgentID: "agent", HarnessID: "test", ProjectID: "p", UpstreamID: "native-1"}
	if err := s.SaveSession(original); err != nil {
		t.Fatal(err)
	}
	if err := s.ArchiveSession("chat", "agent", "earlier"); err != nil {
		t.Fatal(err)
	}
	original.UpstreamID = "native-2"
	if err := s.SaveSession(original); err != nil {
		t.Fatal(err)
	}
	pending, err := s.BeginSessionCloseChecked(t.Context(), original, "started", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSession(original); !errors.Is(err, ErrSessionClosing) {
		t.Fatalf("late save removed the durable close fence: %v", err)
	}
	if _, err := s.RestoreSession("chat", "agent", 1); !errors.Is(err, ErrSessionClosing) {
		t.Fatalf("history restore replaced a closing session: %v", err)
	}
	if _, err := ExportProject(s.doc, "p", []string{"chat"}); !errors.Is(err, ErrSessionClosing) {
		t.Fatalf("transfer moved a pending native close: %v", err)
	}
	if err := s.FinishSessionClose(t.Context(), pending, nil); err != nil {
		t.Fatal(err)
	}
	restored, err := s.RestoreSession("chat", "agent", 1)
	if err != nil || restored.UpstreamID != original.UpstreamID || restored.ClosingAt != "" {
		t.Fatalf("confirmed close prevented deliberate history restoration: %+v, %v", restored, err)
	}
}
