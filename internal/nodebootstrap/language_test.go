package nodebootstrap

import (
	"bytes"
	"errors"
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

// An installer the owner chose that cannot be sent is refused in the
// owner's language when the owner is told.
func TestBinaryRefusalsAreSaidInTheOwnersLanguage(t *testing.T) {
	dir := t.TempDir()
	unknown := filepath.Join(dir, "node")
	if err := os.WriteFile(unknown, bytes.Repeat([]byte("not a binary"), 10), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		locale i18n.Locale
		wantEN bool
	}{{i18n.LocaleEN, true}, {i18n.LocaleZH, false}} {
		for _, path := range []string{filepath.Join(dir, "missing"), dir, unknown} {
			_, err := InspectBinary(path)
			var refusal BinaryRefusal
			if !errors.As(err, &refusal) {
				t.Fatalf("%s refusal = %v", path, err)
			}
			if said := refusal.Say(i18n.New(tc.locale)); containsHan(said) == tc.wantEN {
				t.Errorf("%s refusal %q is not in that language", tc.locale, said)
			}
		}
	}
}

// A refusal made where nobody's language is known is English: only the
// place that answers the owner puts it in the owner's language.
func TestBinaryRefusalsAreEnglishUntilSomeoneIsTold(t *testing.T) {
	dir := t.TempDir()
	unknown := filepath.Join(dir, "node")
	if err := os.WriteFile(unknown, bytes.Repeat([]byte("not a binary"), 10), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(dir, "missing"), dir, unknown} {
		_, err := InspectBinary(path)
		if err == nil || containsHan(err.Error()) {
			t.Errorf("%s refusal %v is not English", path, err)
		}
	}
}
