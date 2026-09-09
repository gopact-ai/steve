package tui

import (
	"testing"
	"unicode/utf8"

	"github.com/gopact-ai/steve/internal/text"
)

// truncate fits a string into a column width, ellipsis included; text.Clip
// keeps a number of runes and adds the ellipsis on top. The two agree on
// most inputs, and this pins the ones where they do not, so neither
// replaces the other by accident.
func TestTruncateIsNotClip(t *testing.T) {
	for _, tc := range []struct {
		name  string
		s     string
		width int
		want  string
	}{
		{"fits", "abc", 8, "abc"},
		{"exactly width stays whole", "abcdefgh", 8, "abcdefgh"},
		{"one over is cut to width", "abcdefghi", 8, "abcdefg…"},
		{"runes not bytes", "一二三四五六七八九", 5, "一二三四…"},
		{"narrow width is clamped to four", "abcdefgh", 1, "abc…"},
		{"clamped width still returns a short string whole", "abcd", 0, "abcd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := truncate(tc.s, tc.width)
			if got != tc.want {
				t.Fatalf("truncate(%q, %d) = %q, want %q", tc.s, tc.width, got, tc.want)
			}
			if n := utf8.RuneCountInString(got); n > max(tc.width, 4) {
				t.Fatalf("truncate(%q, %d) is %d columns wide", tc.s, tc.width, n)
			}
		})
	}

	// Clip(s, width-1) is the nearest candidate and differs at the boundary:
	// a string exactly width runes long fits truncate and is cut by Clip.
	if got, clip := truncate("abcdefgh", 8), text.Clip("abcdefgh", 7); got == clip {
		t.Fatalf("truncate and Clip(s, width-1) agree on an exact fit: %q", got)
	}
	// Clip(s, width) keeps the whole width and adds the ellipsis after it,
	// one column too wide.
	if clip := text.Clip("abcdefghi", 8); utf8.RuneCountInString(clip) != 9 {
		t.Fatalf("Clip(s, width) = %q, expected width+1 runes", clip)
	}
	// Neither clamps: below four columns Clip cuts as told.
	if clip := text.Clip("abcdefgh", 1); clip != "a…" {
		t.Fatalf("Clip(s, 1) = %q", clip)
	}
}
