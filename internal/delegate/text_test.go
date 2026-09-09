package delegate

import (
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/text"
)

// The package used to carry its own rune clip and first-line cut. These
// are those implementations, kept to show text.Clip and text.FirstLine
// answer the same for every input the package feeds them.
func legacyClipRunes(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit]) + "…"
}

func legacyGoal(s string) string {
	trimmed := strings.TrimSpace(s)
	if line, _, found := strings.Cut(trimmed, "\n"); found {
		trimmed = strings.TrimSpace(line)
	}
	if len([]rune(trimmed)) <= goalLimit {
		return trimmed
	}
	return string([]rune(trimmed)[:goalLimit]) + "…"
}

func TestClipMatchesTheClipDelegateHadForEveryLimitItUses(t *testing.T) {
	inputs := []string{
		"", "a", "plain ascii answer", "中文回答，超过限制就截断", "mixed 中英 text with emoji 🎉🎉🎉",
		strings.Repeat("x", 199), strings.Repeat("x", 200), strings.Repeat("x", 201),
		strings.Repeat("字", 300), strings.Repeat("字", 301), strings.Repeat("回答 ", 2000),
		"line one\nline two", "\r\ncrlf\r\n",
	}
	for _, limit := range []int{0, 1, 2, 120, 200, 300, 4000} {
		for _, in := range inputs {
			if got, want := text.Clip(in, limit), legacyClipRunes(in, limit); got != want {
				t.Errorf("Clip(%q, %d) = %q, want %q", in, limit, got, want)
			}
		}
	}
}

func TestGoalMatchesTheGoalDelegateHad(t *testing.T) {
	inputs := []string{
		"", "   ", "write it", "  write it  \n and more", "first\r\nsecond", "\n\nleading blank line",
		strings.Repeat("目标", 60), strings.Repeat("目标", 61), strings.Repeat("g", 119) + "\nrest",
		strings.Repeat("g", 121) + "\nrest", "  " + strings.Repeat("g", 120) + "  \n",
	}
	for _, in := range inputs {
		if got, want := goal(in), legacyGoal(in); got != want {
			t.Errorf("goal(%q) = %q, want %q", in, got, want)
		}
	}
}
