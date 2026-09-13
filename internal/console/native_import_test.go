package console

import (
	"errors"
	"github.com/gopact-ai/steve/internal/i18n"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/nativehistory"
)

func TestNativeImportReceiptSurvivesTranscriptPruningAndRestart(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	s := New(&echo{}, "owner", nil)
	if err := s.Persist(book.Document("console")); err != nil {
		t.Fatal(err)
	}
	origin := consoleapi.ImportedSession{Node: "worker", Project: "p", Agent: "agent", Reference: nativehistory.Reference{ID: "selected", Harness: "codex", NativeID: "native"}}
	if _, err := s.EnsureImportedConversation(t.Context(), "key", origin, func(string) error { return errors.New("binding unavailable") }); err == nil || len(s.Conversations()) != 0 {
		t.Fatal("failed import published a conversation")
	}
	bound := 0
	first, err := s.EnsureImportedConversation(i18n.WithLocale(t.Context(), i18n.LocaleEN), "key", origin, func(string) error { bound++; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.Replies(first.Conversation)[0].Text, "History imported") {
		t.Fatal("English import notice was not localized")
	}
	if len(s.exchanges) != 0 || len(s.running) != 0 {
		t.Fatal("import started work")
	}
	s.mu.Lock()
	for range keep + 1 {
		s.recordLocked(consoleapi.Reply{Conversation: first.Conversation, Text: "later reply"})
	}
	err = s.save()
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	restored := New(&echo{}, "owner", nil)
	if err := restored.Persist(book.Document("console")); err != nil {
		t.Fatal(err)
	}
	second, err := restored.EnsureImportedConversation(t.Context(), "key", origin, func(string) error { bound++; return nil })
	if err != nil || first != second || bound != 1 {
		t.Fatalf("lost import receipt: %+v %v bindings=%d", second, err, bound)
	}
	lookup := consoleapi.NativeImportRequest{CommandID: "key", Project: origin.Project, Agent: origin.Agent, Source: nativehistory.Source{Harness: origin.Reference.Harness, Home: origin.Reference.SourceHome}, NativeID: origin.Reference.NativeID, Revision: origin.Reference.Revision}
	got, found, err := restored.ImportedConversation(origin.Node, lookup)
	if err != nil || !found || got != first {
		t.Fatalf("durable lookup: %+v %v %v", got, found, err)
	}
	lookup.Project = "different"
	if _, _, err := restored.ImportedConversation(origin.Node, lookup); err == nil {
		t.Fatal("lookup changed destination")
	}
	origin.Agent = "other"
	if _, err := restored.EnsureImportedConversation(t.Context(), "key", origin, func(string) error { return nil }); err == nil {
		t.Fatal("same command changed destination")
	}
}
