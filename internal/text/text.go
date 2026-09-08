// Package text holds the small pure string helpers that several packages
// used to carry their own copies of. It depends on nothing but the
// standard library so any package can use it.
package text

import "strings"

// Ellipsis marks where Clip cut a string.
const Ellipsis = "…"

// Clip keeps at most limit runes of s and marks the cut with an Ellipsis.
// A string within the limit comes back unchanged; a limit below zero
// keeps nothing, like zero does.
func Clip(s string, limit int) string {
	count := 0
	for i := range s {
		if count >= limit {
			return s[:i] + Ellipsis
		}
		count++
	}
	return s
}

// FirstLine is s up to, not including, its first "\n". A "\r" before it
// stays: the line is cut, not trimmed.
func FirstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
