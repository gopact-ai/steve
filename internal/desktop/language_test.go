package desktop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"

	"github.com/gopact-ai/steve/internal/i18n"
)

// containsHan reports text left in Chinese where English is expected.
func containsHan(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return unicode.Is(unicode.Han, r) }) >= 0
}

// A workspace the owner cannot use is refused in the owner's language.
func TestWorkspaceRefusalsAreSaidInTheOwnersLanguage(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	file := filepath.Join(home, "notes.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(home, "Library", "Application Support", "Steve")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		locale i18n.Locale
		wantEN bool
	}{{i18n.LocaleEN, true}, {i18n.LocaleZH, false}} {
		text := i18n.New(tc.locale)
		var said []string
		for _, path := range []string{"", "relative/dir", home, "/usr/local/steve", file, string([]byte{'/', 'a', 0}), state} {
			_, err := PrepareWorkspace(text, path, state)
			if err == nil {
				t.Fatalf("%q should be refused", path)
			}
			said = append(said, err.Error())
		}
		for _, err := range []error{CheckWorkspaceProject(text, "", true, ""), CheckWorkspaceProject(text, "default", false, "far")} {
			said = append(said, err.Error())
		}
		for _, message := range said {
			if message == "" || containsHan(message) == tc.wantEN {
				t.Errorf("%s refusal %q is not in that language", tc.locale, message)
			}
		}
	}
}
