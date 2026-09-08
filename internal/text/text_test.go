package text

import "testing"

func TestClip(t *testing.T) {
	for _, tc := range []struct {
		in    string
		limit int
		want  string
	}{
		{"", 3, ""},
		{"abc", 3, "abc"},
		{"abcd", 3, "abc…"},
		{"你好世界", 2, "你好…"},
		{"你好", 2, "你好"},
		{"ab", 0, "…"},
		{"ab", -1, "…"},
		{"", -1, ""},
		{"a\xffb", 2, "a\xff…"},
	} {
		if got := Clip(tc.in, tc.limit); got != tc.want {
			t.Errorf("Clip(%q, %d) = %q, want %q", tc.in, tc.limit, got, tc.want)
		}
	}
}

func TestFirstLine(t *testing.T) {
	for in, want := range map[string]string{
		"":              "",
		"one":           "one",
		"one\ntwo":      "one",
		"\ntwo":         "",
		"one\r\ntwo":    "one\r",
		"one\n\nthree ": "one",
	} {
		if got := FirstLine(in); got != want {
			t.Errorf("FirstLine(%q) = %q, want %q", in, got, want)
		}
	}
}
