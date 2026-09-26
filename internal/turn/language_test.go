package turn

import (
	"errors"
	"strings"
	"testing"
	"unicode"

	"github.com/gopact-ai/steve/internal/agentexec"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/i18n"
)

// containsHan reports text left in Chinese where English is expected.
func containsHan(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return unicode.Is(unicode.Han, r) }) >= 0
}

// A retained execution the coordinator cannot rejoin is asked about in the
// request's language: what was tried, what blocks it, why, and what to do.
func TestRetainedRecoveryQuestionIsAskedWhollyInTheRequestLanguage(t *testing.T) {
	c, _, _, record, req := retainedChatFixture(t)
	release, err := c.SealIdle()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	for _, tc := range []struct {
		locale string
		want   string
	}{{"en", "The coordination service cannot rejoin executions for now."}, {"zh", "协调服务暂时不能接续执行。"}} {
		req.Locale = tc.locale
		_, err := c.ResumeRetainedChat(t.Context(), record.ID, req)
		var blocked *agentexec.RecoveryBlocked
		if !errors.As(err, &blocked) || !strings.Contains(blocked.Question.Message, tc.want) {
			t.Fatalf("%s: err = %v", tc.locale, err)
		}
		if tc.locale == "en" && containsHan(blocked.Question.Title+blocked.Question.Message) {
			t.Fatalf("english question keeps Chinese: %+v", blocked.Question)
		}
	}
}

// A target that cannot take the original settings says which, in the
// request's language, before any task text reaches it.
func TestRecoveryPreferencesRefuseInTheRequestLanguage(t *testing.T) {
	err := applyRecoveryPreferences(t.Context(), i18n.New(i18n.LocaleEN), &fakeRunner{}, &attempt.SessionPreferences{Model: "m"})
	if err == nil || containsHan(err.Error()) || !strings.Contains(err.Error(), "model") {
		t.Fatalf("err = %v", err)
	}
}
