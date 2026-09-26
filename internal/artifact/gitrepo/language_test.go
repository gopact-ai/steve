package gitrepo

import (
	"strings"
	"testing"
	"unicode"

	"github.com/gopact-ai/steve/internal/i18n"
)

// containsHan reports text left in Chinese where English is expected.
func containsHan(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return unicode.Is(unicode.Han, r) }) >= 0
}

// An exceeded snapshot budget is explained in the reader's language, with
// the sizes and the remedy; the error itself carries no reader's language.
func TestSnapshotBudgetIsExplainedInTheReadersLanguage(t *testing.T) {
	for _, which := range []string{"files", "bytes", "file_bytes"} {
		limit := TooLarge{Which: which, Have: 30, Limit: 20}
		en, zh := limit.Say(i18n.New(i18n.LocaleEN)), limit.Say(i18n.New(i18n.LocaleZH))
		if containsHan(en) || !containsHan(zh) || containsHan(limit.Error()) {
			t.Fatalf("%s: en %q, zh %q, error %q", which, en, zh, limit.Error())
		}
		for _, said := range []string{en, zh, limit.Error()} {
			if !strings.Contains(said, "30") || !strings.Contains(said, "20") || !strings.Contains(said, ".gitignore") {
				t.Fatalf("%s: %q lacks the sizes or the remedy", which, said)
			}
		}
	}
}
