package conversationapi

import (
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/view"
	"slices"
	"strings"
	"sync"
)

const (
	answerKept   = 8000
	toolTextKept = 3000
	toolsKept    = 60
)

// FromProgress cuts a turn's progress down to what is worth sending.
func FromProgress(p view.Progress) Progress {
	out := Progress{
		Agent: p.Agent, Node: p.Settings.Node, Model: p.Settings.Model,
		Reasoning: p.Reasoning, Answer: tailText(p.Answer, answerKept),
	}
	limit := toolsKept
	if len(p.Timeline) > 0 {
		// Every timeline reference needs a corresponding tool detail.
		limit = 0
	}
	out.Tools = toolCalls(p.Tools, 0, limit)
	for _, s := range p.Timeline {
		out.Timeline = append(out.Timeline, Span{Kind: s.Kind, Text: s.Text, Tool: s.Tool, At: s.At})
	}
	for _, s := range p.Plan {
		out.Plan = append(out.Plan, PlanLine{Text: s.Text, Status: string(s.Status)})
	}
	return out
}

// The platform's own tools are recognised whatever a harness calls them
// — claude-code says mcp__steve__steve_fleet, codex mcp.steve.steve_fleet
// — and shown by their label, as kind "platform". The catalogue comes
// from the messaging server itself, so a new tool needs no page change;
// each label is said in the language text gives when the call is shown.
var platform struct {
	mu     sync.RWMutex
	server string
	titles map[string]i18n.Key
	text   i18n.Catalog
}

// SetPlatformTools installs the messaging server's name and tool labels,
// and the catalog that says the labels.
func SetPlatformTools(server string, titles map[string]i18n.Key, text i18n.Catalog) {
	platform.mu.Lock()
	defer platform.mu.Unlock()
	platform.server = strings.ToLower(server)
	platform.text = text
	platform.titles = map[string]i18n.Key{}
	for name, title := range titles {
		platform.titles[strings.ToLower(name)] = title
	}
}

// platformTool says whether a harness's tool name is one of the
// platform's, and what to call it.
func platformTool(raw string) (name, title string, ok bool) {
	platform.mu.RLock()
	defer platform.mu.RUnlock()
	if platform.server == "" || len(platform.titles) == 0 {
		return "", "", false
	}
	lower := strings.ToLower(strings.TrimSpace(raw))
	if title, found := platform.titles[lower]; found {
		return lower, platform.text.T(title), true
	}
	// Split on every separator a harness uses between server and tool,
	// but never on a single underscore: that is inside the tool's name.
	parts := strings.FieldsFunc(strings.ReplaceAll(lower, "__", "\x00"), func(r rune) bool {
		return r == '\x00' || r == '.' || r == '/' || r == ':'
	})
	for i, p := range parts {
		if p != platform.server || i+1 >= len(parts) {
			continue
		}
		tail := strings.Join(parts[i+1:], "_")
		if title, found := platform.titles[tail]; found {
			return tail, platform.text.T(title), true
		}
	}
	return "", "", false
}

func toolCalls(tools []view.Tool, depth, limit int) []ToolCall {
	var out []ToolCall
	for _, t := range tools {
		if limit > 0 && len(out) >= limit {
			break
		}
		call := ToolCall{
			ID: t.ID, Kind: t.Kind, Name: t.Name, Detail: t.Detail, Status: t.Status,
			Input: headText(t.Input, toolTextKept), Output: headText(t.Output, toolTextKept),
		}
		if name, title, ok := platformTool(t.Name); ok {
			call.Kind, call.Name, call.Detail = "platform", name, title
		}
		out = append(out, call)
		if depth < 2 {
			out = append(out, toolCalls(t.Children, depth+1, limit)...)
		}
	}
	return out
}

func tailText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

func headText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// FromStepProgress uses the same projection for live events and durable steps.
func FromStepProgress(id string, p Progress, info StepInfo) StepProcess {
	if info.Answer == "" {
		info.Answer = p.Answer
	}
	return CloneStep(StepProcess{ID: id, Agent: p.Agent, Node: p.Node, Model: p.Model,
		Reasoning: p.Reasoning, Tools: p.Tools, Plan: p.Plan, Timeline: p.Timeline, StepInfo: info})
}

// CloneStep owns producer buffers before a snapshot becomes observable history.
func CloneStep(step StepProcess) StepProcess {
	step.Tools = slices.Clone(step.Tools)
	step.Plan = slices.Clone(step.Plan)
	step.Timeline = slices.Clone(step.Timeline)
	step.Refs = slices.Clone(step.Refs)
	return step
}
