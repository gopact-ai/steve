package memory

import (
	"context"
	"strings"
	"testing"
	"unicode"

	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/ledger"
)

// A memory file the owner has not written yet is shown, and started, from
// a template that explains itself in the Hub's language. The section
// headings facts are filed under are the same in every language.
func TestMemoryTemplateExplainsItselfInTheHubLanguage(t *testing.T) {
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	locale := home.LocaleEN
	source := func() home.Locale { return locale }
	markdown := NewMarkdown(t.TempDir(), t.TempDir())
	markdown.SetLocale(source)
	shared := NewLedgerStore(book)
	shared.SetLocale(source)
	for _, store := range []struct {
		name string
		Store
		Text func(context.Context, Scope) (string, error)
	}{{"markdown", markdown, markdown.Text}, {"ledger", shared, shared.Text}} {
		for _, want := range []home.Locale{home.LocaleEN, home.LocaleZH} {
			locale = want
			for _, scope := range []Scope{Global, ProjectScope("unwritten-" + store.name + "-" + string(want))} {
				text, err := store.Text(t.Context(), scope)
				if err != nil {
					t.Fatal(err)
				}
				if explained := explanation(text); hasHan(explained) != (want == home.LocaleZH) {
					t.Errorf("%s %s template in %s explains itself as %q", store.name, scope, want, explained)
				}
				for _, section := range Sections(scope) {
					if !strings.Contains(text, "## "+section+"\n") {
						t.Errorf("%s %s template in %s lacks section %s: %q", store.name, scope, want, section, text)
					}
				}
			}
		}
	}
	locale = home.LocaleEN
	scope := ProjectScope("remembered")
	if _, err := markdown.Remember(t.Context(), scope, "", "the build needs go 1.25"); err != nil {
		t.Fatal(err)
	}
	if text, err := markdown.Text(t.Context(), scope); err != nil || hasHan(explanation(text)) {
		t.Errorf("a file started by remembering explains itself as %q (%v)", explanation(text), err)
	}
}

// explanation is a template's prose: every line that is neither a
// heading nor a remembered fact.
func explanation(text string) string {
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") && !strings.HasPrefix(line, "- ") {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

func hasHan(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return unicode.Is(unicode.Han, r) }) >= 0
}
