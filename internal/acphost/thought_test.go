package acphost

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestThoughtKeepsFullTextUntil64KiB(t *testing.T) {
	c := &collector{}
	want := strings.Repeat("x", maxThoughtBytes)
	c.writeThought(want[:9000])
	c.writeThought(want[9000:])
	if got, _ := c.snapshot(); got.Reasoning != want {
		t.Fatalf("thought was silently truncated: got %d, want %d bytes", len(got.Reasoning), len(want))
	}
}

func TestThoughtOverflowKeepsHeadAndMovingTail(t *testing.T) {
	marker := regexp.MustCompile(`\n\[… 省略 (\d+) 字节 …\]\n`)
	for _, unit := range []string{"x", "思考", "🧠a"} {
		t.Run(unit, func(t *testing.T) {
			c := &collector{}
			full := ""
			for _, chunk := range []string{strings.Repeat(unit, 20000), strings.Repeat(unit, 65537), unit, unit, unit, "最新的结尾"} {
				full += chunk
				c.writeThought(chunk)
				got, _ := c.snapshot()
				if len(got.Reasoning) > maxThoughtBytes || !utf8.ValidString(got.Reasoning) {
					t.Fatalf("invalid or unbounded thought: %d bytes", len(got.Reasoning))
				}
				if len(full) <= maxThoughtBytes {
					if got.Reasoning != full {
						t.Fatal("lost text before the cap")
					}
					continue
				}
				loc := marker.FindStringSubmatchIndex(got.Reasoning)
				if loc == nil {
					t.Fatal("missing explicit omission marker")
				}
				head, tail := got.Reasoning[:loc[0]], got.Reasoning[loc[1]:]
				skipped, _ := strconv.Atoi(got.Reasoning[loc[2]:loc[3]])
				if !strings.HasPrefix(full, head) || !strings.HasSuffix(full, tail) || len(head) < 32000 || len(tail) < 32000 || skipped != len(full)-len(head)-len(tail) {
					t.Fatalf("head/tail or omitted byte count is wrong: head=%d tail=%d skipped=%d total=%d", len(head), len(tail), skipped, len(full))
				}
			}
		})
	}
}
