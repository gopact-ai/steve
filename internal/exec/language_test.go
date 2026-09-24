package exec

import "unicode"

// containsHan reports text left in Chinese where English is expected.
func containsHan(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}
