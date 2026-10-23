package consoleclient

import "testing"

// The terminal's locale reaches the server as a tag it can parse, so
// replies follow the user's language.
func TestAcceptLanguageFromPOSIXLocale(t *testing.T) {
	for _, c := range []struct {
		locales []string
		want    string
	}{
		{[]string{"", "en_US.UTF-8"}, "en-US"},
		{[]string{"zh_CN.UTF-8", "en_US.UTF-8"}, "zh-CN"},
		{[]string{"C.UTF-8", "de_DE@euro"}, "de-DE"},
		{[]string{"POSIX", ""}, ""},
	} {
		if got := acceptLanguage(c.locales...); got != c.want {
			t.Errorf("acceptLanguage(%q) = %q, want %q", c.locales, got, c.want)
		}
	}
}
