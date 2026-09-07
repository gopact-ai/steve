package i18n

import "testing"

func TestHeaderNegotiationHonorsPriorityAndExclusions(t *testing.T) {
	for header, want := range map[string]Locale{
		"zh-CN,en;q=0.8":       LocaleZH,
		"en;q=0,zh;q=0.8":      LocaleZH,
		"zh;q=0.2,en-US;q=0.9": LocaleEN,
		"fr,en-US;q=0.8":       LocaleEN,
		"fr":                   "",
		"":                     "",
	} {
		if got := LocaleFromHeader(header); got != want {
			t.Errorf("%q: %q, want %q", header, got, want)
		}
	}
}
