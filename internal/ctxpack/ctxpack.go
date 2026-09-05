// Package ctxpack assembles what a step's agent gets to see, and bounds it.
//
// Three tempting answers are all wrong. Handing over the parent's full
// history is expensive and mostly irrelevant — the other agent has a
// different harness, a different window, and no use for someone else's tool
// calls. Handing over nothing makes it redo the exploration. Letting the
// model decide what to pass puts the whole thing back on model goodwill, and
// leaves nothing to inspect afterwards.
//
// So the payload is explicit and typed: a goal, the ancestry of goals above
// it, resolvable refs, prior findings, and a bearings section. It is bounded,
// and it is persisted with the step, which is what makes "what did that agent
// actually see?" a question with an answer.
package ctxpack

import (
	"fmt"
	"strings"

	"github.com/gopact-ai/steve/internal/plan"
)

// MaxBytes bounds one assembled context. Past it the payload must be turned
// into a ref — written to the repo or the blob store — rather than truncated.
// Truncation loses the end of the thing quietly; a ref keeps all of it and
// says where.
const MaxBytes = 24 << 10

// Context is what one step's agent is given.
type Context struct {
	Goal string `json:"goal"`
	// Ancestry is the chain of goals above this step, outermost first. It is
	// the goals, never the transcripts: enough to know what this serves.
	Ancestry []string `json:"ancestry,omitempty"`
	// Refs point at state instead of carrying it.
	Refs []plan.Ref `json:"refs,omitempty"`
	// Findings are what earlier steps learned.
	Findings []plan.Finding `json:"findings,omitempty"`
	// Bearings orients a cold start — an agent that begins by reading where
	// things stand does not redo work or declare finished work unfinished.
	Bearings string `json:"bearings,omitempty"`
	// Facts are the machine's own capabilities, so the agent knows what it
	// can reach from where it is running.
	Facts []string `json:"facts,omitempty"`
	// TurnsLeft and Deadline are the budget as the agent should understand
	// it: enough to pace itself, not a suggestion it can ignore.
	TurnsLeft int    `json:"turns_left,omitempty"`
	Deadline  string `json:"deadline,omitempty"`
}

// ErrTooLarge says the payload must be turned into a ref first.
type ErrTooLarge struct {
	Size int
	Max  int
}

func (e ErrTooLarge) Error() string {
	return fmt.Sprintf("context is %d bytes, over the %d limit — put the bulk in a ref (a commit or a blob) and pass the pointer",
		e.Size, e.Max)
}

// Build assembles and checks the payload. It returns ErrTooLarge rather than
// trimming: silently shipping less than the caller asked for is how an agent
// ends up missing the one paragraph that mattered.
func Build(c Context) (Context, error) {
	rendered := c.Render()
	if len(rendered) > MaxBytes {
		return Context{}, ErrTooLarge{Size: len(rendered), Max: MaxBytes}
	}
	return c, nil
}

// Render is the text form handed to the agent. It reads as instructions
// because that is what it is; the struct is what gets persisted and shown.
func (c Context) Render() string {
	var b strings.Builder
	b.WriteString(c.Goal)
	if len(c.Ancestry) > 0 {
		b.WriteString("\n\n## 这一步服务于\n")
		for i, goal := range c.Ancestry {
			fmt.Fprintf(&b, "%s%s\n", strings.Repeat("  ", i), goal)
		}
	}
	if c.Bearings != "" {
		b.WriteString("\n\n## 先定向再动手\n")
		b.WriteString(c.Bearings)
	}
	if len(c.Refs) > 0 {
		b.WriteString("\n\n## 可用的引用\n")
		for _, ref := range c.Refs {
			fmt.Fprintf(&b, "- %s: %s", ref.Kind, ref.Value)
			if ref.Note != "" {
				fmt.Fprintf(&b, " — %s", ref.Note)
			}
			b.WriteString("\n")
		}
	}
	if len(c.Findings) > 0 {
		b.WriteString("\n\n## 前序步骤的发现\n")
		for _, f := range c.Findings {
			fmt.Fprintf(&b, "- %s\n", f.Text)
		}
	}
	if len(c.Facts) > 0 {
		fmt.Fprintf(&b, "\n\n## 这台机器\n%s\n", strings.Join(c.Facts, ", "))
	}
	if c.TurnsLeft > 0 || c.Deadline != "" {
		b.WriteString("\n\n## 预算\n")
		if c.TurnsLeft > 0 {
			fmt.Fprintf(&b, "剩余轮次 %d。", c.TurnsLeft)
		}
		if c.Deadline != "" {
			fmt.Fprintf(&b, "截止 %s。", c.Deadline)
		}
		b.WriteString("\n只推进这一步，做完就交回，不要顺手做别的。\n")
	}
	return b.String()
}

// Bearings is the cold-start routine. Every step starts in a session with no
// memory of the last one, so the first thing it must do is find out where
// things stand — otherwise it either redoes finished work or declares
// unfinished work done.
func Bearings(workspace string, prior []plan.Ref) string {
	var b strings.Builder
	b.WriteString("动手之前先看一眼现状：\n")
	if workspace != "" {
		fmt.Fprintf(&b, "- 工作目录 %s。空目录、没有 git 仓库都是正常的起点，直接在这里做，不要因此停下\n", workspace)
	}
	b.WriteString("- 目录里如果已经有 git 仓库，看 `git log --oneline -10` 与 `git status` 了解它处于什么状态；没有就跳过\n")
	for _, ref := range prior {
		if ref.Kind == "git" {
			fmt.Fprintf(&b, "- 前一步留在 %s\n", ref.Value)
		}
	}
	b.WriteString("- 只有当目标本身说不通（要改的文件不存在、要求互相矛盾）才停下来说明；环境跟预想不同不算\n")
	return b.String()
}
