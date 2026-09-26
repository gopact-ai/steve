package i18n

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A sentence that tells the reader which button to press names it
// exactly as the button is labelled in the same language, in quotes.
func TestSentencesNameButtonsAsTheyAreLabelled(t *testing.T) {
	for _, locale := range []Locale{LocaleZH, LocaleEN} {
		table := zh
		if locale == LocaleEN {
			table = en
		}
		for _, key := range []Key{RetainedAdviceSealOpen, ConsoleRecoveryStopUnfinished, ConsoleRecoveryRepeated} {
			if !quotes(table[key], table[ConsoleStopChoice]) {
				t.Errorf("%s %s = %q, want the stop choice %q quoted", locale, key, table[key], table[ConsoleStopChoice])
			}
		}
		ssh := consoleLabel(t, locale, "ssh", "ssh.connect")
		if !quotes(table[AdminNodeBinaryMissingNote], ssh) {
			t.Errorf("%s %s = %q, want the console's SSH button %q quoted", locale, AdminNodeBinaryMissingNote, table[AdminNodeBinaryMissingNote], ssh)
		}
	}
}

// quotes reports whether text names label in quotes: 「」 or “” as the
// Chinese sentences have them, straight double quotes in English.
func quotes(text, label string) bool {
	for _, pair := range [][2]string{{"「", "」"}, {"“", "”"}, {`"`, `"`}} {
		if strings.Contains(text, pair[0]+label+pair[1]) {
			return true
		}
	}
	return false
}

// consoleLabel is the web console's label for key in locale.
func consoleLabel(t *testing.T, locale Locale, file, key string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "web", "console", "src", "lib", "i18n", string(locale), file+".ts"))
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`"` + regexp.QuoteMeta(key) + `":\s*"([^"]*)"`).FindSubmatch(raw)
	if match == nil {
		t.Fatalf("the console has no %s label %s", locale, key)
	}
	return string(match[1])
}
