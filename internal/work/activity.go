package work

import (
	"strings"

	"github.com/gopact-ai/steve/internal/view"
)

// Stage is the executor's own plan, as far as the harness reported one.
type Stage struct {
	Done    int    `json:"done"`
	Total   int    `json:"total"`
	Current string `json:"current,omitempty"`
}

// Activity is what a card shows about running work. It is read from the
// harness's events — tool calls, plan steps, usage — not from anything the
// model chose to report, so it stays right when the model reports nothing.
type Activity struct {
	Line          string `json:"line,omitempty"`
	Stage         *Stage `json:"stage,omitempty"`
	Tokens        uint64 `json:"tokens,omitempty"`
	ContextTokens uint64 `json:"context_tokens,omitempty"`
	ContextWindow uint64 `json:"context_window,omitempty"`
	Model         string `json:"model,omitempty"`
}

const maxLine = 120

// activityOf reads one progress report. The line is the newest tool still
// running, else the newest tool, else the start of the answer being written.
func activityOf(p view.Progress) Activity {
	a := Activity{Model: p.Settings.Model}
	if p.Usage.Reported {
		a.Tokens = p.Usage.TotalTokens
		if a.Tokens == 0 {
			a.Tokens = p.Usage.InputTokens + p.Usage.OutputTokens
		}
		a.ContextTokens, a.ContextWindow = p.Usage.ContextTokens, p.Usage.ContextWindow
	}
	if tool, ok := newestTool(p.Tools); ok {
		a.Line = toolLine(tool)
	} else if answer := strings.TrimSpace(p.Answer); answer != "" {
		a.Line = "Writing: " + firstLine(answer)
	}
	if len(p.Plan) > 0 {
		s := &Stage{Total: len(p.Plan)}
		for _, step := range p.Plan {
			if step.Status == view.StepCompleted {
				s.Done++
			} else if s.Current == "" {
				s.Current = clip(step.Text)
			}
		}
		a.Stage = s
	}
	a.Line = clip(a.Line)
	return a
}

func newestTool(tools []view.Tool) (view.Tool, bool) {
	for i := len(tools) - 1; i >= 0; i-- {
		if tools[i].Status == view.ToolRunning {
			return tools[i], true
		}
	}
	if len(tools) > 0 {
		return tools[len(tools)-1], true
	}
	return view.Tool{}, false
}

func toolLine(t view.Tool) string {
	name := strings.TrimSpace(t.Name)
	if name == "" {
		name = strings.TrimSpace(t.Kind)
	}
	return firstLine(name)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func clip(s string) string {
	r := []rune(s)
	if len(r) <= maxLine {
		return s
	}
	return string(r[:maxLine-1]) + "…"
}
