package console

import (
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/ledger"
)

func TestEnsureRecoveryConversationIsVisibleDurableAndDoesNotSubmitWork(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	s := New(&echo{}, "owner", nil)
	if err := s.Persist(book.Document("console")); err != nil {
		t.Fatal(err)
	}
	source := RecoveryConversation{ParentTaskID: "parent-1", SourceChannel: "feishu", SourceConversation: "oc_original", Project: "p"}
	first, err := s.EnsureRecoveryConversation(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	if first != "console:recovery:parent-1" {
		t.Fatalf("unstable recovery conversation %q", first)
	}
	if second, err := s.EnsureRecoveryConversation(t.Context(), source); err != nil || second != first {
		t.Fatalf("retry = %q %v", second, err)
	}
	summaries := s.Summaries(t.Context())
	if len(summaries) != 1 || summaries[0].ID != first || !strings.Contains(summaries[0].Title, "飞书") || !strings.Contains(summaries[0].Title, "parent-1") {
		t.Fatalf("GUI cannot identify recovery source: %+v", summaries)
	}
	replies := s.Replies(first)
	if len(replies) != 1 || replies[0].Kind != "notice" || replies[0].Format != "text" || !strings.Contains(replies[0].Text, source.SourceConversation) || replies[0].ProjectID != "p" {
		t.Fatalf("source notice = %+v", replies)
	}
	if len(s.exchanges) != 0 || len(s.questions) != 0 || len(s.running) != 0 {
		t.Fatal("creating a visible recovery page started work")
	}
	restored := New(&echo{}, "owner", nil)
	if err := restored.Persist(book.Document("console")); err != nil {
		t.Fatal(err)
	}
	if id, err := restored.EnsureRecoveryConversation(t.Context(), source); err != nil || id != first || len(restored.Replies(id)) != 1 {
		t.Fatalf("restart duplicated recovery page: %q %v", id, err)
	}
	source.SourceConversation = "oc_other"
	if _, err := restored.EnsureRecoveryConversation(t.Context(), source); err == nil {
		t.Fatal("existing parent recovery page accepted another source")
	}
}

func TestEnsureRecoveryConversationRetriesStorageFailureWithoutPhantomPage(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	s := New(&echo{}, "owner", nil)
	if err := s.Persist(book.Document("console")); err != nil {
		t.Fatal(err)
	}
	source := RecoveryConversation{ParentTaskID: "parent-2", SourceChannel: "feishu", SourceConversation: "oc_original", Project: "p"}
	if _, err := book.DB().Exec("PRAGMA query_only = ON"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsureRecoveryConversation(t.Context(), source); err == nil {
		t.Fatal("uncommitted page reported success")
	}
	if len(s.Conversations()) != 0 || len(s.meta) != 0 {
		t.Fatal("failed persistence left a phantom GUI conversation")
	}
	if _, err := book.DB().Exec("PRAGMA query_only = OFF"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsureRecoveryConversation(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	if len(s.Conversations()) != 1 {
		t.Fatal("retry could not create the original page")
	}
}
