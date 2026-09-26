package gateway

import (
	"reflect"
	"strings"
	"testing"
	"unicode"

	"github.com/gopact-ai/steve/internal/i18n"
)

// A card adds no words of its own, so the gateway fills every one, in
// the reader's language.
func TestTurnCopyFillsEveryWordInEachLanguage(t *testing.T) {
	for _, locale := range []i18n.Locale{i18n.LocaleZH, i18n.LocaleEN} {
		copy := reflect.ValueOf(turnCopy(i18n.New(locale)))
		for i := 0; i < copy.NumField(); i++ {
			word := copy.Field(i).String()
			name := copy.Type().Field(i).Name
			if word == "" {
				t.Errorf("%s: %s is empty", locale, name)
			}
			if locale == i18n.LocaleEN && strings.IndexFunc(word, func(r rune) bool { return unicode.Is(unicode.Han, r) }) >= 0 {
				t.Errorf("en: %s = %q", name, word)
			}
		}
	}
}
