package delegate

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/view"
)

// containsHan reports text left in Chinese where English is expected.
func containsHan(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return unicode.Is(unicode.Han, r) }) >= 0
}

// A child that cannot be joined is asked about in the service's language:
// the title, what was tried, what blocks it, why, and both choices.
func TestRecoveryQuestionIsAskedInTheServiceLanguage(t *testing.T) {
	for _, tc := range []struct {
		locale i18n.Locale
		tried  string
	}{{i18n.LocaleEN, "Tried: "}, {i18n.LocaleZH, "已尝试："}} {
		t.Run(string(tc.locale), func(t *testing.T) {
			w, sessions, _, _ := detachedDelegateFixture(t)
			sessions.inspectErr = errors.New("node unreachable")
			service := recoveredDelegateServiceIn(t, w, sessions, i18n.New(tc.locale))
			notices := make(chan RecoveryQuestion, 1)
			service.SetRecoveryQuestion(func(_ context.Context, q RecoveryQuestion) (view.Answer, error) {
				notices <- q
				return view.Answer{Value: "wait"}, nil
			})
			if err := service.RecoverRetained(t.Context()); err != nil {
				t.Fatal(err)
			}
			var q view.Question
			select {
			case got := <-notices:
				q = got.Question
			case <-time.After(3 * time.Second):
				t.Fatal("missing recovery question")
			}
			if !strings.HasPrefix(q.Message, tc.tried) || len(q.Choices) != 2 {
				t.Fatalf("question = %+v", q)
			}
			if tc.locale != i18n.LocaleEN {
				return
			}
			parts := []string{q.Title, q.Message}
			for _, c := range q.Choices {
				parts = append(parts, c.Label, c.Detail)
			}
			for _, part := range parts {
				if containsHan(part) {
					t.Fatalf("English question keeps Chinese: %q", part)
				}
			}
		})
	}
}

// The line the person sees for a returned child is in the service's
// language; the parent agent's prompt is not the person's.
func TestDeliveryNoticeIsInTheServiceLanguage(t *testing.T) {
	children := []Delivered{
		{Task: "7", Agent: "claude", Node: "hub", State: task.StateCancelled, Elapsed: 3 * time.Second},
		{Task: "8", Agent: "codex", Node: "hub", State: task.StateFailed, Elapsed: time.Second},
		{Task: "9", Agent: "codex", Node: "hub", State: task.StateDone, Elapsed: time.Second},
	}
	en := Delivery{Children: children, text: i18n.New(i18n.LocaleEN)}.Notice()
	if containsHan(en) || !strings.Contains(en, "#7 cancelled") || !strings.Contains(en, "#8 failed") || !strings.Contains(en, "#9 done") {
		t.Fatalf("English notice = %q", en)
	}
	zh := Delivery{Children: children, text: i18n.New(i18n.LocaleZH)}.Notice()
	if !strings.Contains(zh, "⤵ 子任务 #7 已取消") {
		t.Fatalf("Chinese notice = %q", zh)
	}
}

// A child that never reached the ledger ends with an answer in the
// service's language; it is shown with the child's result.
func TestUnrecordedChildAnswersInTheServiceLanguage(t *testing.T) {
	w, _ := executionWorld(t)
	parent := w.running(t, "codex")
	child := unrecordedChild(t, w, parent, false)
	service := recoveredDelegateServiceIn(t, w, w.sessions, i18n.New(i18n.LocaleEN))
	service.SetDeliverer((&mailbox{}).deliver)
	if err := service.RecoverRetained(t.Context()); err != nil {
		t.Fatal(err)
	}
	stored, _ := w.tasks.Get(child.ID)
	if stored.Result == nil || stored.Result.Answer == "" || containsHan(stored.Result.Answer) {
		t.Fatalf("result = %+v", stored.Result)
	}
}

// A child whose node process stopped is ended with an explanation in the
// service's language.
func TestStoppedChildIsExplainedInTheServiceLanguage(t *testing.T) {
	w, sessions, _, child := detachedDelegateFixture(t)
	sessions.mu.Lock()
	sessions.processStopped = true
	sessions.mu.Unlock()
	service := recoveredDelegateServiceIn(t, w, sessions, i18n.New(i18n.LocaleEN))
	service.SetDeliverer(func(context.Context, Delivery) error { return nil })
	if err := service.RecoverRetained(t.Context()); err != nil {
		t.Fatal(err)
	}
	stored := awaitDelegateResult(t, w.tasks, child.TaskID)
	if stored.Result.Answer == "" || containsHan(stored.Result.Answer) {
		t.Fatalf("stopped child explained as %q, want English", stored.Result.Answer)
	}
}
