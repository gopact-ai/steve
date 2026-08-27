package agentmcp

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	maxContentRunes = 6000
	maxSummaryRunes = 100
)

// atTag matches Feishu mention markup in both card markdown and text
// messages. Content arrives from the agent, so mentions are stripped rather
// than trusted: pinging people is the platform's final card's job alone.
var atTag = regexp.MustCompile(`(?i)<\s*/?\s*at\b[^>]*>`)

func stripMentions(s string) string {
	return atTag.ReplaceAllString(s, "")
}

// milestoneCard renders an agent's interim message as a minimal Card 2.0:
// the markdown body plus a quiet source line, deliberately unlike the
// platform's own turn card so the two cannot be confused.
func milestoneCard(markdown, agentID string) []byte {
	elements := []map[string]any{
		{
			"tag":     "markdown",
			"content": markdown,
			"margin":  "0px 0px 8px 0px",
		},
	}
	if agentID != "" {
		elements = append(elements, map[string]any{
			"tag":       "markdown",
			"text_size": "notation",
			"content":   "<font color='grey'>" + agentID + "</font>",
			"margin":    "0px",
		})
	}
	out := map[string]any{
		"schema": "2.0",
		"config": map[string]any{
			"update_multi": true,
			"width_mode":   "default",
			"summary":      map[string]any{"content": summaryLine(markdown)},
		},
		"body": map[string]any{
			"direction": "vertical",
			"padding":   "12px",
			"elements":  elements,
		},
	}
	raw, err := json.Marshal(out)
	if err != nil {
		// The value is built from plain strings; this cannot fail, but a
		// panic in a tool call would take the whole gateway down.
		return []byte(`{"schema":"2.0","body":{"elements":[]}}`)
	}
	return raw
}

func summaryLine(markdown string) string {
	line := strings.Join(strings.Fields(markdown), " ")
	return truncateRunes(line, maxSummaryRunes)
}

func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max]) + "…"
}
