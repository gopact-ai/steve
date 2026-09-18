// Package approval turns the owner's approval stance into the mode each AI
// tool actually offers. Every harness names its approval levels itself —
// Codex says read-only / agent / agent-full-access, Claude Code says
// default / acceptEdits / auto / bypassPermissions — so a fleet-wide
// default cannot be one of those words. It is an intent, and each agent's
// own list of modes is what it is resolved against.
package approval

import "strings"

// The three stances an owner can take, in the order they give away.
const (
	// Ask stops at every command and edit.
	Ask = "ask"
	// Auto lets the agent work but keeps the dangerous doors closed.
	Auto = "auto"
	// Full approves everything, including what reaches outside the
	// workspace.
	Full = "full"
)

// Intents are the accepted settings values; "" means no fleet-wide default,
// which leaves every agent on whatever its tool starts in.
func Intents() []string { return []string{"", Ask, Auto, Full} }

func Valid(intent string) bool {
	switch intent {
	case "", Ask, Auto, Full:
		return true
	}
	return false
}

// Mode is one approval level a harness offers: the value it answers to and
// the name a person reads beside it.
type Mode struct {
	Value string
	Label string
}

// Rank places one mode on the ask → auto → full ladder, or returns "" for a
// mode that is not an approval level at all. Planning modes are the reason
// that last case exists: Claude Code lists "plan" beside its approval
// levels, and no approval stance should ever park an agent in it.
func Rank(mode Mode) string {
	for _, text := range []string{strings.ToLower(strings.TrimSpace(mode.Value)), strings.ToLower(strings.TrimSpace(mode.Label))} {
		if text == "" {
			continue
		}
		if intent := classify(text); intent != "" {
			return intent
		}
	}
	return ""
}

// classify reads one word the way its author meant it. Order matters: the
// most permissive words are checked first, because "agent-full-access"
// contains "agent" and "Full access" is not a workspace-only stance.
func classify(text string) string {
	switch {
	case text == "plan" || strings.Contains(text, "planning"):
		return ""
	case contains(text, "full-access", "full access", "fullaccess", "bypass", "yolo", "danger"):
		return Full
	case contains(text, "read-only", "readonly", "read only", "manual", "untrusted", "suggest", "ask", "chat", "approval") || text == "default" || text == "read":
		return Ask
	case contains(text, "accept", "auto", "agent", "approve", "write", "edit", "workspace"):
		return Auto
	}
	return ""
}

func contains(text string, words ...string) bool {
	for _, word := range words {
		if strings.Contains(text, word) {
			return true
		}
	}
	return false
}

// Resolve picks the mode that carries out the intent, in the order the
// agent listed them. Nothing is approximated: a tool that offers no mode at
// that level reports no match, and the caller leaves it alone rather than
// guessing a stance its owner did not choose.
func Resolve(intent string, modes []Mode) (Mode, bool) {
	if intent == "" {
		return Mode{}, false
	}
	for _, mode := range modes {
		if mode.Value == "" {
			continue
		}
		if Rank(mode) == intent {
			return mode, true
		}
	}
	return Mode{}, false
}
