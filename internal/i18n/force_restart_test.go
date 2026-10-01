package i18n

import (
	"strings"
	"testing"
)

func TestForceStopConfirmationWarnsAboutOtherExecutions(t *testing.T) {
	for _, tc := range []struct {
		locale Locale
		words  string
	}{{LocaleZH, "同节点上的其他执行会被中断"}, {LocaleEN, "other executions on that node will be interrupted"}} {
		message := New(tc.locale).T(ConsoleForceConfirmBody, "1", "original")
		if !strings.Contains(message, tc.words) {
			t.Fatalf("%s restart side effect missing: %s", tc.locale, message)
		}
	}
}
