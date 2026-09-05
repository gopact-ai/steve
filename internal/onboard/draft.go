package onboard

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/i18n"
)

type Draft struct {
	Soul string
	User string
}

const (
	soulFence = "===SOUL.md==="
	userFence = "===USER.md==="
)

func AllowScan(input string) bool {
	folded := strings.ToLower(strings.TrimSpace(input))
	for _, phrase := range []string{
		"不允许", "不要扫", "别扫", "不要扫描", "不准扫", "别扫描",
		"don't scan", "do not scan", "no scan", "dont scan",
	} {
		if strings.Contains(folded, phrase) {
			return false
		}
	}
	return true
}

func Building(conversationID string, ownerP2P, needsInit bool) bool {
	if !ownerP2P || !needsInit {
		return false
	}
	return !strings.HasPrefix(conversationID, "steve:onboard:")
}

func Continue(locale i18n.Locale, homePath, excerpts string) string {
	lang := "简体中文"
	if locale == i18n.LocaleEN {
		lang = "English"
	}
	var b strings.Builder
	b.WriteString("The owner answered. Build their profile now.\n")
	b.WriteString("Working directory: ")
	b.WriteString(homePath)
	b.WriteString("\nWrite both files in ")
	b.WriteString(lang)
	b.WriteString(".\n")
	b.WriteString("Rules:\n")
	b.WriteString("- Do not use tools. Steve will write the files from your output.\n")
	b.WriteString("- Do not invent. Only use the owner's message and the session excerpts below.\n")
	b.WriteString("- USER.md is a durable portrait: name, timezone, projects, preferences, people. Short bullets.\n")
	b.WriteString("- SOUL.md is Steve's identity as their personal assistant. The AI tools Steve drives are hands, not another self; do not name specific tools, Steve is told what it has each turn.\n")
	b.WriteString("- Do not put channel identifiers (a Feishu open_id, a token) in USER.md; they live in Steve's config. Do not include the template marker comment.\n")
	b.WriteString("- First write a short Feishu reply confirming what you recorded.\n")
	b.WriteString("- Then output ONLY this shape:\n\n")
	b.WriteString(soulFence)
	b.WriteString("\n<full SOUL.md>\n")
	b.WriteString(userFence)
	b.WriteString("\n<full USER.md>\n")
	if excerpts != "" {
		b.WriteString("\nSession excerpts (stable facts only):\n")
		b.WriteString(excerpts)
	}
	return b.String()
}

func ParseDraft(out string) (Draft, error) {
	soul := section(out, soulFence, userFence)
	user := after(out, userFence)
	soul = strings.TrimSpace(soul)
	user = strings.TrimSpace(user)
	if soul == "" || user == "" {
		return Draft{}, fmt.Errorf("onboard: draft missing SOUL.md or USER.md")
	}
	if !usableDraft(soul, user) {
		return Draft{}, fmt.Errorf("onboard: draft is a placeholder")
	}
	return Draft{Soul: soul + "\n", User: user + "\n"}, nil
}

func usableDraft(soul, user string) bool {
	if strings.Contains(soul, "<full SOUL.md>") || strings.Contains(user, "<full USER.md>") {
		return false
	}
	if home.IsTemplate(soul) || home.IsTemplate(user) {
		return false
	}
	return len(soul) >= 20 && len(user) >= 8
}

func Apply(homePath, out string) (reply string, written bool, err error) {
	draft, err := ParseDraft(out)
	if err != nil {
		return strings.TrimSpace(stripDraft(out)), false, nil
	}
	if err := home.WriteIdentity(homePath, draft.Soul, draft.User); err != nil {
		return "", false, err
	}
	return strings.TrimSpace(stripDraft(out)), true, nil
}

func stripDraft(out string) string {
	if i := strings.Index(out, soulFence); i >= 0 {
		out = out[:i]
	}
	if i := strings.Index(out, userFence); i >= 0 && i < len(out) {
		out = out[:i]
	}
	return strings.TrimRightFunc(out, unicode.IsSpace)
}

func section(body, start, end string) string {
	i := strings.Index(body, start)
	if i < 0 {
		return ""
	}
	body = body[i+len(start):]
	if j := strings.Index(body, end); j >= 0 {
		body = body[:j]
	}
	return body
}

func after(body, start string) string {
	i := strings.Index(body, start)
	if i < 0 {
		return ""
	}
	return body[i+len(start):]
}
