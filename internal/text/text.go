// Package text holds the small pure string helpers that several packages
// used to carry their own copies of. It depends on nothing but the
// standard library so any package can use it.
package text

import (
	"strings"
	"unicode/utf8"
)

// Ellipsis marks where Clip cut a string.
const Ellipsis = "…"

// Clip keeps at most limit runes of s and marks the cut with an Ellipsis.
// A string within the limit comes back unchanged.
func Clip(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	return string([]rune(s)[:limit]) + Ellipsis
}

// FirstLine is s up to, not including, its first newline.
func FirstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
