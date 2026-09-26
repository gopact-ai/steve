package console

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/readmodel"
	"github.com/gopact-ai/steve/internal/schedule"
)

func containsHan(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return unicode.Is(unicode.Han, r) }) >= 0
}

// The page opened for a question from another channel is titled and
// introduced in the console's language, and stays the same page when that
// language changes afterwards.
func TestRecoveryConversationSpeaksTheConsoleLanguage(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	s := impatient(New(&echo{}, "owner", nil))
	s.SetDefaultLocale("en")
	if err := s.PersistLedger(book); err != nil {
		t.Fatal(err)
	}
	source := RecoveryConversation{ParentTaskID: "parent-1", SourceChannel: "feishu", SourceConversation: "oc_original", Project: "p"}
	id, err := s.EnsureRecoveryConversation(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	summaries := s.Summaries(t.Context())
	replies := s.Replies(id)
	if len(summaries) != 1 || containsHan(summaries[0].Title) || !strings.Contains(summaries[0].Title, "Feishu") {
		t.Fatalf("title = %+v", summaries)
	}
	if len(replies) != 1 || containsHan(replies[0].Text) || !strings.Contains(replies[0].Text, "oc_original") {
		t.Fatalf("notice = %+v", replies)
	}
	s.SetDefaultLocale("zh")
	if again, err := s.EnsureRecoveryConversation(t.Context(), source); err != nil || again != id {
		t.Fatalf("after a language change = %q %v", again, err)
	}
}

// A scheduled line on the page is labelled in the console's language.
func TestScheduledLineIsLabelledInTheConsoleLanguage(t *testing.T) {
	h := &queueHandler{started: make(chan *queueCall, 8)}
	s := New(h, "owner", nil)
	s.SetDefaultLocale("en")
	if err := s.Persist(&memDoc{}); err != nil {
		t.Fatal(err)
	}
	s.SetInspector(&scheduledInspector{project: "p"})
	f := schedule.Firing{Job: schedule.Job{ID: "1", Channel: "console", ConversationID: "console:work", ProjectID: "p", Requester: "owner", Member: "builder", Prompt: "scheduled work"}, Key: "schedule:1:fixed-time", ScheduledAt: time.Now()}
	e, err := s.EnqueueScheduled(t.Context(), f)
	if err != nil {
		t.Fatal(err)
	}
	if containsHan(e.Input) || !strings.Contains(e.Input, "#1") || !strings.Contains(e.Input, "scheduled work") {
		t.Fatalf("input = %q", e.Input)
	}
}

// Refusing to rewrite a line Steve relayed is said in the submission's
// language.
func TestRelayedLineRefusalIsInTheSubmissionLanguage(t *testing.T) {
	h := &rewindable{}
	s := New(h, "ou_owner", readmodel.New(readmodel.Sources{}))
	ctx := context.Background()
	if err := s.Continue(ctx, "main", "k1", "claude", "⤵ Subtask #12 done", "child is back"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, s, "console:main")
	var relayed consoleapi.Reply
	for _, reply := range s.Replies("main") {
		if reply.Kind == "sent" && reply.Relayed {
			relayed = reply
		}
	}
	_, err := s.Submit(ctx, consoleapi.Submission{Conversation: "main", Input: "rewritten", CommandID: "c1", RewindTo: relayed.ID, Locale: "en"})
	if err == nil || containsHan(err.Error()) {
		t.Fatalf("err = %v", err)
	}
}

// The questions a stuck recovery asks are in the exchange's language.
func TestStopChoiceIsInTheExchangeLanguage(t *testing.T) {
	for locale, want := range map[string]string{"en": "Stop and cancel the original run", "zh": "停止并取消原执行"} {
		if got := stopChoice(i18n.New(i18n.FromLang(locale))); got.Value != "stop" || got.Label != want {
			t.Fatalf("%s choice = %+v", locale, got)
		}
	}
}
