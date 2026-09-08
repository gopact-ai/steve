package turn

import (
	"regexp"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/state"
)

func TestHistoryListIsNumberedWithoutMarkdownListSyntax(t *testing.T) {
	got := historyList([]state.Archived{
		{Session: state.Session{UpstreamID: "01a03e6d-f610-7c00-8e05-6e80d349ce30"}, ArchivedAt: "2026-08-26T14:59:50Z"},
		{Session: state.Session{UpstreamID: "short"}, ArchivedAt: "not-a-time"},
	})
	// A leading "N." makes Feishu render an ordered list, which absorbs the
	// instruction line that follows the block.
	if regexp.MustCompile(`(?m)^\d+\.`).MatchString(got) {
		t.Fatalf("numbering would be read as a markdown list:\n%s", got)
	}
	if !strings.Contains(got, "[1] ") || !strings.Contains(got, "[2] ") {
		t.Fatalf("entries not numbered:\n%s", got)
	}
	if !strings.Contains(got, "01a03e6d…") {
		t.Fatalf("long id not shortened:\n%s", got)
	}
	// An unparseable timestamp is shown as-is rather than dropped.
	if !strings.Contains(got, "not-a-time") || !strings.Contains(got, "short") {
		t.Fatalf("unparseable entry mangled:\n%s", got)
	}
}
