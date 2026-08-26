// Package card renders one Feishu Card 2.0 for a Steve turn.
package card

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gopact-ai/steve/internal/view"
)

// The turn's shape lives in internal/view so that renderers depend on the
// semantics and not the other way round. These aliases keep card.X working for
// callers that are already inside the Feishu path.
type (
	Status     = view.Status
	ToolStatus = view.ToolStatus
	Phase      = view.Phase
	Tool       = view.Tool
	Usage      = view.Usage
	Field      = view.Field
	Progress   = view.Progress
	Turn       = view.Turn
	Approval   = view.Approval
	Settings   = view.Settings
	Question   = view.Question
	Choice     = view.Choice
	Step       = view.Step
	StepStatus = view.StepStatus
)

const (
	StatusRunning   = view.StatusRunning
	StatusCompleted = view.StatusCompleted
	StatusFailed    = view.StatusFailed
	StatusCancelled = view.StatusCancelled

	ToolRunning   = view.ToolRunning
	ToolCompleted = view.ToolCompleted
	ToolFailed    = view.ToolFailed

	PhaseWaking  = view.PhaseWaking
	PhaseRunning = view.PhaseRunning

	StepPending    = view.StepPending
	StepInProgress = view.StepInProgress
	StepCompleted  = view.StepCompleted
)

const (
	MaxBytes           = 28 * 1024
	maxAnswerRunes     = 6000
	maxErrorRunes      = 600
	maxToolName        = 80
	maxToolDetail      = 240
	maxToolIO          = 800
	maxVisibleTools    = 6
	maxSummaryRunes    = 100
	maxReasoningRunes  = 1000
	maxReasoningLines  = 12
	maxFieldLabel      = 40
	maxFieldValue      = 240
	maxApprovalReason  = 240
	maxSettingRunes    = 32
	maxVisibleSteps    = 20
	maxStepRunes       = 240
	maxChoices         = 4
	maxChoiceLabel     = 40
	maxQuestionRunes   = 400
	headerIconToken    = "myai_colorful"
	cardActionApproval = "tool_approval"
	cardActionQuestion = "elicit_answer"
	cardActionRecover  = "history_restore"
	cardActionCancel   = "turn_cancel"
	cardActionRetry    = "turn_retry"
)

type Copy struct {
	Title          string
	Running        string
	Completed      string
	Failed         string
	Cancelled      string
	EarlierTools   string
	Execution      string
	Plan           string
	EarlierSteps   string
	Input          string
	Output         string
	Context        string
	In             string
	Out            string
	Hit            string
	Write          string
	Awaiting       string
	ApprovalTitle  string
	ApprovalTool   string
	ApprovalReason string
	ApprovalRule   string
	QuestionTitle  string
	QuestionHint   string
	Recover        string
	AllowOnce      string
	Deny           string
	Waking         string
	Stop           string
	Retry          string
}

func Render(t Turn, copy Copy) []byte {
	t = bound(t)
	raw := mustJSON(build(t, copy))
	for len(raw) > MaxBytes {
		if t.Answer != "" {
			t.Answer = shrinkRunes(t.Answer, utf8.RuneCountInString(t.Answer)*3/4)
		} else if len(t.Tools) > 1 {
			t.Tools = t.Tools[1:]
		} else if t.Error != "" {
			t.Error = shrinkRunes(t.Error, utf8.RuneCountInString(t.Error)*3/4)
		} else {
			break
		}
		raw = mustJSON(build(t, copy))
	}
	return raw
}

func bound(t Turn) Turn {
	t.Answer = truncateRunes(t.Answer, maxAnswerRunes)
	t.Reasoning = visibleReasoning(t.Reasoning)
	t.Error = truncateRunes(t.Error, maxErrorRunes)
	t.Tools = boundTools(t.Tools)
	t.Plan, t.PlanHidden = boundSteps(t.Plan)
	for i := range t.Fields {
		t.Fields[i].Label = truncateRunes(t.Fields[i].Label, maxFieldLabel)
		t.Fields[i].Value = truncateRunes(t.Fields[i].Value, maxFieldValue)
	}
	if t.UpdatedAt.IsZero() {
		t.UpdatedAt = t.StartedAt
	}
	if t.Approval != nil {
		t.Approval.ToolName = truncateRunes(t.Approval.ToolName, maxToolName)
		t.Approval.Reason = truncateRunes(t.Approval.Reason, maxApprovalReason)
	}
	t.Question = boundQuestion(t.Question)
	return t
}

func build(t Turn, copy Copy) map[string]any {
	elements := make([]map[string]any, 0, 4)
	elements = append(elements, fieldBlocks(t)...)
	if plan := planBlock(t, copy); plan != nil {
		elements = append(elements, plan)
	}
	if panel := executionPanel(t, copy); panel != nil {
		elements = append(elements, panel)
	}
	elements = append(elements, approvalBlocks(t, copy)...)
	elements = append(elements, questionBlocks(t, copy)...)
	if t.Answer != "" {
		elements = append(elements, markdown("answer", formatMarkdown(t.Answer), "normal"))
	}
	if t.Error != "" {
		elements = append(elements, highlight("error", "red-50", "red-100", escape(t.Error)))
	}
	if control := controlRow(t, copy); control != nil {
		elements = append(elements, control)
	}
	if row := recoverRow(t, copy); row != nil {
		elements = append(elements, row)
	}
	elements = append(elements, footer(t, copy))
	for i, el := range elements {
		if i == len(elements)-1 {
			el["margin"] = "0px"
			continue
		}
		el["margin"] = "0px 0px 12px 0px"
	}

	width := "default"
	if len(t.Fields) > 0 && t.Answer == "" {
		width = "compact"
	}
	out := map[string]any{
		"schema": "2.0",
		"config": map[string]any{
			"update_multi": true,
			"width_mode":   width,
			"summary":      map[string]any{"content": truncateRunes(summary(t, copy), maxSummaryRunes)},
		},
		"body": map[string]any{
			"direction":          "vertical",
			"padding":            "12px 12px 20px 12px",
			"horizontal_spacing": "8px",
			"vertical_spacing":   "8px",
			"elements":           elements,
		},
	}
	if h := header(t, copy); h != nil {
		out["header"] = h
	}
	return out
}

// header carries the card's state as colour: a running turn is blue, one
// waiting on a human is orange, a failed one red — scannable from the chat
// list without reading a word. A completed plain answer drops the banner,
// because a finished archive no longer has a state worth announcing; only
// titled results (status reports) keep their green header.
func header(t Turn, copy Copy) map[string]any {
	title := strings.TrimSpace(t.Title)
	if title == "" {
		if t.Status == StatusCompleted {
			return nil
		}
		title = copy.Title
		if title == "" {
			title = "Steve"
		}
	}
	label, template, tagColor := headerTone(t, copy)
	h := map[string]any{
		"title":    map[string]any{"tag": "plain_text", "content": title},
		"template": template,
		"icon":     map[string]any{"tag": "standard_icon", "token": headerIconToken},
		"text_tag_list": []map[string]any{
			{
				"tag":   "text_tag",
				"text":  map[string]any{"tag": "plain_text", "content": label},
				"color": tagColor,
			},
		},
	}
	if sub := subtitle(t); sub != "" {
		h["subtitle"] = map[string]any{"tag": "plain_text", "content": sub}
	}
	return h
}

func headerTone(t Turn, copy Copy) (label, template, tagColor string) {
	if t.Approval != nil && t.Approval.RequestID != "" && t.Status == StatusRunning {
		label = copy.Awaiting
		if label == "" {
			label = "待授权"
		}
		return label, "orange", "orange"
	}
	if t.Status == StatusRunning && t.Phase == PhaseWaking && copy.Waking != "" {
		return copy.Waking, "blue", "yellow"
	}
	switch t.Status {
	case StatusCompleted:
		return copy.Completed, "green", "green"
	case StatusFailed:
		return copy.Failed, "red", "red"
	case StatusCancelled:
		return copy.Cancelled, "grey", "neutral"
	default:
		return copy.Running, "blue", "yellow"
	}
}

func fieldBlocks(t Turn) []map[string]any {
	metrics, details := splitFields(t.Fields)
	blocks := make([]map[string]any, 0, 2)
	if len(metrics) > 0 {
		blocks = append(blocks, metricSet(t.Status, metrics))
	}
	if len(details) > 0 {
		blocks = append(blocks, detailPanel(details))
	}
	return blocks
}

func splitFields(fields []Field) (metrics, details []Field) {
	for _, field := range fields {
		if field.IsMetric {
			metrics = append(metrics, field)
			continue
		}
		details = append(details, field)
	}
	if len(metrics) == 0 {
		var shorts []Field
		rest := make([]Field, 0, len(details))
		for _, field := range details {
			if !field.Wide && len(shorts) < 2 {
				shorts = append(shorts, field)
				continue
			}
			rest = append(rest, field)
		}
		if len(shorts) > 0 && len(rest) > 0 {
			return shorts, rest
		}
	}
	if len(metrics) > 3 {
		details = append(append([]Field{}, metrics[3:]...), details...)
		metrics = metrics[:3]
	}
	return metrics, details
}

func metricSet(status Status, fields []Field) map[string]any {
	color, bg, border := metricColors(status)
	columns := make([]map[string]any, 0, len(fields))
	for i, field := range fields {
		columns = append(columns, map[string]any{
			"tag":    "column",
			"width":  "weighted",
			"weight": 1,
			"elements": []map[string]any{
				metricCard(fmt.Sprintf("metric%d", i), field.Label, field.Value, color, bg, border),
			},
		})
	}
	return map[string]any{
		"tag":                "column_set",
		"element_id":         "metrics",
		"flex_mode":          "none",
		"horizontal_spacing": "8px",
		"columns":            columns,
	}
}

func metricCard(id, label, value, color, bg, border string) map[string]any {
	return map[string]any{
		"tag":              "interactive_container",
		"element_id":       id,
		"width":            "fill",
		"has_border":       true,
		"border_color":     border,
		"corner_radius":    "8px",
		"background_style": bg,
		"padding":          "12px 12px 12px 12px",
		"vertical_spacing": "2px",
		"elements": []map[string]any{
			{
				"tag":        "markdown",
				"content":    fmt.Sprintf("## <font color='%s'>%s</font>", color, escape(value)),
				"text_align": "center",
			},
			{
				"tag":        "markdown",
				"content":    fmt.Sprintf("<font color='grey'>%s</font>", escape(label)),
				"text_align": "center",
				"text_size":  "notation",
			},
		},
	}
}

func metricColors(status Status) (color, bg, border string) {
	switch status {
	case StatusCompleted:
		return "green", "green-50", "green-100"
	case StatusFailed:
		return "red", "red-50", "red-100"
	case StatusCancelled:
		return "grey", "grey-50", "grey"
	default:
		return "blue", "blue-50", "blue-100"
	}
}

func detailPanel(fields []Field) map[string]any {
	return map[string]any{
		"tag":              "interactive_container",
		"element_id":       "details",
		"width":            "fill",
		"has_border":       true,
		"border_color":     "grey",
		"corner_radius":    "8px",
		"background_style": "grey-50",
		"padding":          "12px 12px 12px 12px",
		"vertical_spacing": "4px",
		"elements":         []map[string]any{fieldsDiv(fields)},
	}
}

func fieldsDiv(fields []Field) map[string]any {
	items := make([]map[string]any, 0, len(fields))
	for _, field := range fields {
		items = append(items, map[string]any{
			"is_short": !field.Wide,
			"text": map[string]any{
				"tag":     "lark_md",
				"content": fmt.Sprintf("**<font color='grey'>%s</font>**\n%s", escape(field.Label), escape(field.Value)),
			},
		})
	}
	return map[string]any{
		"tag":        "div",
		"element_id": "fields",
		"fields":     items,
	}
}

func highlight(id, background, border, content string) map[string]any {
	return map[string]any{
		"tag":              "interactive_container",
		"element_id":       id,
		"width":            "fill",
		"has_border":       true,
		"border_color":     border,
		"corner_radius":    "8px",
		"background_style": background,
		"padding":          "12px 12px 12px 12px",
		"vertical_spacing": "4px",
		"elements":         []map[string]any{markdown("", content, "normal")},
	}
}

// boundSteps keeps the tail of a long plan: the steps an agent is working on
// now sit at the end, and those are the ones worth the card's space.
func boundSteps(steps []Step) ([]Step, int) {
	hidden := 0
	if len(steps) > maxVisibleSteps {
		hidden = len(steps) - maxVisibleSteps
		steps = steps[hidden:]
	}
	out := make([]Step, 0, len(steps))
	for _, step := range steps {
		step.Text = truncateRunes(step.Text, maxStepRunes)
		out = append(out, step)
	}
	return out, hidden
}

func boundTools(tools []Tool) []Tool {
	for i := range tools {
		tools[i].Kind = truncateRunes(tools[i].Kind, maxToolName)
		tools[i].Name = truncateRunes(tools[i].Name, maxToolDetail)
		tools[i].Detail = truncateRunes(tools[i].Detail, maxToolDetail)
		tools[i].Input = truncateRunes(tools[i].Input, maxToolIO)
		tools[i].Output = truncateRunes(tools[i].Output, maxToolIO)
		tools[i].Children = boundTools(tools[i].Children)
	}
	return tools
}

// planBlock draws the plan the agent said it is working to. It sits above the
// execution panel because it is what the tool calls below are working
// through.
func planBlock(t Turn, copy Copy) map[string]any {
	if len(t.Plan) == 0 {
		return nil
	}
	title := copy.Plan
	if title == "" {
		title = "Plan"
	}
	lines := make([]string, 0, len(t.Plan))
	for _, step := range t.Plan {
		text := strings.TrimSpace(step.Text)
		if text == "" {
			continue
		}
		lines = append(lines, stepMark(step.Status)+" "+escapeText(text))
	}
	if len(lines) == 0 {
		return nil
	}
	done := t.PlanHidden // trimmed steps are the oldest, hence finished ones
	for _, step := range t.Plan {
		if step.Status == StepCompleted {
			done++
		}
	}
	head := fmt.Sprintf("%s %d/%d", title, done, len(t.Plan)+t.PlanHidden)
	// The trim notice is its own element rather than a "<font>" line joined
	// into the steps: a font tag that shares a markdown block with newlines
	// is exactly what leaks a stray "</font>" onto the card.
	elements := make([]map[string]any, 0, 2)
	if t.PlanHidden > 0 {
		earlier := copy.EarlierSteps
		if earlier == "" {
			earlier = "earlier steps"
		}
		elements = append(elements, greyText("", fmt.Sprintf("%d %s", t.PlanHidden, earlier), "notation", 0))
	}
	elements = append(elements, markdown("plan_steps", strings.Join(lines, "\n"), "normal"))
	return collapsible("plan", head, planExpanded(t), elements)
}

// planExpanded keeps the plan open while it is being worked through and
// folds it away once the turn is over, where the answer is what matters.
func planExpanded(t Turn) bool { return t.Status == StatusRunning }

func stepMark(status StepStatus) string {
	switch status {
	case StepCompleted:
		return "✓"
	case StepInProgress:
		return "◉"
	default:
		return "○"
	}
}

func executionPanel(t Turn, copy Copy) map[string]any {
	tools, hidden := visibleTools(t.Tools)
	if len(tools) == 0 && t.Reasoning == "" && t.Status != StatusRunning {
		return nil
	}
	elements := make([]map[string]any, 0, len(tools)+3)
	if t.Reasoning != "" {
		elements = append(elements, greyText("think", t.Reasoning, "notation", maxReasoningLines))
	} else if len(tools) == 0 {
		elements = append(elements, markdown("", "<font color='grey'>"+escape(placeholder(t, copy))+"</font>", "notation"))
	}
	if hidden > 0 && copy.EarlierTools != "" {
		elements = append(elements, markdown("", fmt.Sprintf("<font color='grey'>%d %s</font>", hidden, escape(copy.EarlierTools)), "notation"))
	}
	for i, tool := range tools {
		elements = append(elements, toolPanel(fmt.Sprintf("t%d", i), tool, copy, t.UpdatedAt))
	}
	title := copy.Execution
	if title == "" {
		title = "执行过程"
	}
	return collapsible("exec", "**"+escape(title)+"**", t.Status == StatusRunning, elements)
}

func placeholder(t Turn, copy Copy) string {
	if t.Status == StatusRunning && t.Phase == PhaseWaking {
		if copy.Waking != "" {
			return copy.Waking
		}
		return "Agent 正在唤醒"
	}
	return copy.Running
}

func visibleReasoning(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > maxReasoningLines {
		lines = lines[len(lines)-maxReasoningLines:]
		s = "…" + strings.Join(lines, "\n")
	} else {
		s = strings.Join(lines, "\n")
	}
	if utf8.RuneCountInString(s) <= maxReasoningRunes {
		return s
	}
	return "…" + string([]rune(s)[utf8.RuneCountInString(s)-maxReasoningRunes+1:])
}

func visibleTools(tools []Tool) ([]Tool, int) {
	if len(tools) <= maxVisibleTools {
		return tools, 0
	}
	hidden := len(tools) - maxVisibleTools
	return tools[hidden:], hidden
}

func toolPanel(id string, tool Tool, copy Copy, now time.Time) map[string]any {
	name := toolTitle(tool)
	title := fmt.Sprintf("%s %s **%s**", toolMark(tool.Status), toolEmoji(tool), escapeText(name))
	if d := toolDuration(tool, now); d != "" {
		// Feishu panel headers hold a single text element, so the duration
		// trails the name instead of sitting flush right.
		title += fmt.Sprintf(" <font color='grey'>· %s</font>", d)
	}
	elements := make([]map[string]any, 0, 4+len(tool.Children))
	if meta := toolMeta(tool); meta != "" {
		elements = append(elements, greyText("", meta, "notation", 0))
	}
	if tool.Input != "" {
		label := copy.Input
		if label == "" {
			label = "In"
		}
		elements = append(elements, markdown("", "**"+escape(label)+"**\n"+fence(tool.Input), "notation"))
	}
	if tool.Output != "" {
		label := copy.Output
		if label == "" {
			label = "Out"
		}
		elements = append(elements, markdown("", "**"+escape(label)+"**\n"+fence(tool.Output), "notation"))
	}
	for i, child := range tool.Children {
		elements = append(elements, toolPanel(id+"c"+fmt.Sprint(i), child, copy, now))
	}
	return collapsible(id, title, tool.Status == ToolRunning, elements)
}

// toolTitle labels the collapsed row with what kind of call this is. ACP has
// no separate tool-name field: its title is the agent's own call description,
// which for a shell call is the command itself. Trying to strip the arguments
// back out of that string is guesswork, so prefer the kind the agent already
// classified the call as, and let the command sit inside the panel where its
// full text is readable anyway.
func toolTitle(tool Tool) string {
	if tool.Kind != "" {
		return tool.Kind
	}
	return firstNonEmpty(tool.Name, tool.ID)
}

// toolEmoji gives the collapsed row a glyph so a run of calls stays scannable
// without reading every label.
func toolEmoji(tool Tool) string {
	subject := strings.ToLower(tool.Kind + " " + tool.Name)
	for _, match := range []struct {
		emoji string
		words []string
	}{
		{"⌨️", []string{"execute", "bash", "shell", "command", "terminal"}},
		{"📖", []string{"read", "view"}},
		{"✏️", []string{"edit", "patch"}},
		{"📝", []string{"write", "create"}},
		{"🗂", []string{"delete", "move"}},
		{"🔎", []string{"search", "grep", "find"}},
		{"🌐", []string{"fetch", "web", "http"}},
		{"💭", []string{"think"}},
	} {
		for _, word := range match.words {
			if strings.Contains(subject, word) {
				return match.emoji
			}
		}
	}
	return "🛠"
}

// toolMeta is the agent's own description of the call — for a shell call, the
// command — plus the file it touched. It sits inside the panel rather than
// crowding the collapsed row, which carries the kind instead.
func toolMeta(tool Tool) string {
	parts := make([]string, 0, 2)
	if tool.Name != "" && tool.Name != tool.Kind {
		parts = append(parts, tool.Name)
	}
	if tool.Detail != "" && tool.Detail != tool.Name {
		parts = append(parts, tool.Detail)
	}
	return strings.Join(parts, " · ")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func fence(s string) string {
	s = strings.ReplaceAll(s, "```", "'''")
	return "```\n" + escape(s) + "\n```"
}

func toolDuration(tool Tool, now time.Time) string {
	if tool.StartedAt.IsZero() {
		return ""
	}
	end := tool.UpdatedAt
	if tool.Status == ToolRunning || end.IsZero() {
		end = now
	}
	d := end.Sub(tool.StartedAt)
	if d < 0 {
		d = 0
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

func collapsible(id, title string, expanded bool, elements []map[string]any) map[string]any {
	panel := map[string]any{
		"tag":              "collapsible_panel",
		"element_id":       id,
		"direction":        "vertical",
		"vertical_spacing": "8px",
		"padding":          "12px 12px 12px 12px",
		"background_color": "bg-white",
		"expanded":         expanded,
		"border":           map[string]any{"color": "blue-100", "corner_radius": "8px"},
		"header": map[string]any{
			"title":            map[string]any{"tag": "markdown", "content": title},
			"background_color": "bg-white",
			"vertical_align":   "center",
			"padding":          "8px 8px 8px 8px",
			"icon": map[string]any{
				"tag":   "standard_icon",
				"token": "down-small-ccm_outlined",
				"size":  "16px 16px",
			},
			"icon_position":       "right",
			"icon_expanded_angle": -180,
		},
		"elements": elements,
	}
	return panel
}

// formatMarkdown prepares agent text for a Feishu markdown element.
//
// It used to append "<br>" to lines it judged to be hard-wrapped, on the
// usual markdown rule that a lone newline is only a space. Feishu does not
// follow that rule: a bare newline is already a line break, so the "<br>"
// and the newline each produced one and every such line came out with a
// blank line under it. Probed directly — "A\nB" renders as two lines,
// "A<br>\nB" renders as two lines with a gap.
func formatMarkdown(s string) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return ""
	}
	src := strings.Split(compactHeadings(s), "\n")
	out := make([]string, 0, len(src))
	for _, line := range src {
		out = append(out, escape(line))
	}
	return strings.Join(out, "\n")
}

func compactHeadings(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " ")
		n := 0
		for n < len(trimmed) && trimmed[n] == '#' {
			n++
		}
		if n == 0 || n > 6 || n >= len(trimmed) || trimmed[n] != ' ' {
			continue
		}
		title := strings.TrimSpace(trimmed[n+1:])
		if title == "" {
			continue
		}
		lines[i] = "**" + title + "**"
	}
	return strings.Join(lines, "\n")
}

func toolMark(status ToolStatus) string {
	switch status {
	case ToolCompleted:
		return "✓"
	case ToolFailed:
		return "✕"
	default:
		return "⏳"
	}
}

func summary(t Turn, copy Copy) string {
	return truncateRunes(footerText(t, copy), maxSummaryRunes)
}

func subtitle(t Turn) string {
	for _, field := range t.Fields {
		if field.IsMetric && field.Value != "" {
			return field.Value
		}
	}
	return ""
}

func footer(t Turn, copy Copy) map[string]any {
	return map[string]any{
		"tag":        "column_set",
		"element_id": "meta",
		"flex_mode":  "none",
		"columns": []map[string]any{{
			"tag":    "column",
			"width":  "weighted",
			"weight": 1,
			"elements": []map[string]any{
				markdown("", "<font color='grey'>"+escape(footerText(t, copy))+"</font>", "notation"),
			},
		}},
	}
}

func approvalBlocks(t Turn, copy Copy) []map[string]any {
	if t.Approval == nil || t.Approval.RequestID == "" {
		return nil
	}
	title := copy.ApprovalTitle
	if title == "" {
		title = "需要授权"
	}
	tool := copy.ApprovalTool
	if tool == "" {
		tool = "工具"
	}
	lines := []string{"**" + escape(title) + "**"}
	if t.Approval.ToolName != "" {
		lines = append(lines, escape(tool)+": "+escape(t.Approval.ToolName))
	}
	if t.Approval.Reason != "" {
		reason := copy.ApprovalReason
		if reason == "" {
			reason = "原因"
		}
		lines = append(lines, escape(reason)+": "+escape(t.Approval.Reason))
	}
	rule := copy.ApprovalRule
	if rule == "" {
		rule = "仅允许当前操作一次"
	}
	allow := copy.AllowOnce
	if allow == "" {
		allow = "允许一次"
	}
	deny := copy.Deny
	if deny == "" {
		deny = "拒绝"
	}
	return []map[string]any{
		{
			"tag":              "interactive_container",
			"element_id":       "approval",
			"width":            "fill",
			"has_border":       true,
			"border_color":     "orange-100",
			"corner_radius":    "8px",
			"background_style": "orange-50",
			"padding":          "12px 12px 12px 12px",
			"elements": []map[string]any{
				markdown("", strings.Join(lines, "\n"), "normal"),
				// Its own element, not a "<font>" line joined onto the ones
				// above: a font tag sharing a markdown block with newlines
				// leaks a stray "</font>" onto the card.
				greyText("", rule, "notation", 0),
			},
		},
		{
			"tag":                "column_set",
			"element_id":         "approval_buttons",
			"flex_mode":          "none",
			"horizontal_spacing": "8px",
			"columns": []map[string]any{
				approvalButton("allow", allow, "primary", t.Approval.RequestID, "allow"),
				approvalButton("deny", deny, "default", t.Approval.RequestID, "deny"),
			},
		},
	}
}

// controlRow renders stop while the turn runs and retry once it has failed,
// so the same card carries the only action that makes sense at that moment.
func controlRow(t Turn, copy Copy) map[string]any {
	if t.TurnID == "" || (t.Approval != nil && t.Approval.RequestID != "") {
		return nil
	}
	var label, action, kind string
	switch t.Status {
	case StatusRunning:
		label, action, kind = copy.Stop, cardActionCancel, "danger"
		if label == "" {
			label = "终止"
		}
	case StatusFailed, StatusCancelled:
		label, action, kind = copy.Retry, cardActionRetry, "default"
		if label == "" {
			label = "重试"
		}
	default:
		return nil
	}
	return map[string]any{
		"tag":        "column_set",
		"element_id": "control",
		"flex_mode":  "none",
		"columns": []map[string]any{{
			"tag":    "column",
			"width":  "weighted",
			"weight": 1,
			"elements": []map[string]any{{
				"tag":   "button",
				"name":  action,
				"text":  map[string]any{"tag": "plain_text", "content": label},
				"type":  kind,
				"width": "fill",
				"size":  "medium",
				"behaviors": []map[string]any{{
					"type":  "callback",
					"value": map[string]any{"action": action, "request_id": t.TurnID},
				}},
			}},
		}},
	}
}

// recoverRow puts the way back onto the card that took the session away:
// the /clear confirmation offers one tap to restore what it archived, so
// nobody has to remember that /history exists.
func recoverRow(t Turn, copy Copy) map[string]any {
	if t.RecoverID == "" || t.Status != StatusCompleted {
		return nil
	}
	label := copy.Recover
	if label == "" {
		label = "恢复上个会话"
	}
	return map[string]any{
		"tag":        "column_set",
		"element_id": "recover",
		"flex_mode":  "none",
		"columns": []map[string]any{{
			"tag": "column", "width": "weighted", "weight": 1,
			"elements": []map[string]any{{
				"tag":  "button",
				"name": "recover",
				"text": map[string]any{"tag": "plain_text", "content": label},
				"type": "default", "width": "fill", "size": "medium",
				"behaviors": []map[string]any{{
					"type": "callback",
					"value": map[string]any{
						"action":     cardActionRecover,
						"request_id": t.RecoverID,
					},
				}},
			}},
		}},
	}
}

func approvalButton(id, label, kind, requestID, decision string) map[string]any {
	return map[string]any{
		"tag":            "column",
		"width":          "weighted",
		"weight":         1,
		"vertical_align": "center",
		"elements": []map[string]any{{
			"tag":   "button",
			"name":  id,
			"text":  map[string]any{"tag": "plain_text", "content": label},
			"type":  kind,
			"width": "fill",
			"size":  "medium",
			"behaviors": []map[string]any{{
				"type": "callback",
				"value": map[string]any{
					"action":     cardActionApproval,
					"request_id": requestID,
					"decision":   decision,
				},
			}},
		}},
	}
}

// settingsText names who answered: the harness, the model it ran, and the
// permission mode it ran under. It leads the footer so the chat-list summary
// keeps it even after truncation.
func settingsText(t Turn) []string {
	parts := make([]string, 0, 3)
	for _, value := range []string{t.Settings.Harness, t.Settings.Model, t.Settings.Mode} {
		if value = strings.TrimSpace(value); value != "" {
			parts = append(parts, shrinkRunes(value, maxSettingRunes))
		}
	}
	return parts
}

func footerText(t Turn, copy Copy) string {
	label, _, _ := headerTone(t, copy)
	parts := append(settingsText(t), label, elapsed(t))
	if t.Usage.ContextWindow > 0 {
		ctx := compactTokens(t.Usage.ContextTokens) + "/" + compactTokens(t.Usage.ContextWindow)
		if t.Usage.ContextTokens > 0 {
			ctx += " (" + compactPercent(t.Usage.ContextTokens, t.Usage.ContextWindow) + ")"
		}
		name := copy.Context
		if name == "" {
			name = "Ctx"
		}
		parts = append(parts, name+" "+ctx)
	}
	if t.Usage.InputTokens+t.Usage.CacheReadTokens+t.Usage.CacheWriteTokens+t.Usage.OutputTokens == 0 {
		return strings.Join(parts, " · ")
	}
	in, out, hit, wr := copy.In, copy.Out, copy.Hit, copy.Write
	if in == "" {
		in = "In"
	}
	if out == "" {
		out = "Out"
	}
	if hit == "" {
		hit = "Hit"
	}
	if wr == "" {
		wr = "Wr"
	}
	parts = append(parts, in+" "+compactTokens(t.Usage.InputTokens))
	cacheable := t.Usage.InputTokens + t.Usage.CacheReadTokens + t.Usage.CacheWriteTokens
	if cacheable > 0 {
		hitLine := hit + " " + compactTokens(t.Usage.CacheReadTokens)
		if t.Usage.CacheReadTokens > 0 {
			hitLine += " (" + compactPercent(t.Usage.CacheReadTokens, cacheable) + ")"
		}
		parts = append(parts, hitLine)
	}
	if t.Usage.CacheWriteTokens > 0 {
		parts = append(parts, wr+" "+compactTokens(t.Usage.CacheWriteTokens))
	}
	parts = append(parts, out+" "+compactTokens(t.Usage.OutputTokens))
	return strings.Join(parts, " · ")
}

func compactTokens(n uint64) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 1_000_000:
		return trimFloat(float64(n)/1000) + "K"
	default:
		return trimFloat(float64(n)/1_000_000) + "M"
	}
}

func compactPercent(value, total uint64) string {
	if total == 0 {
		return "0%"
	}
	return trimFloat(float64(value)*100/float64(total)) + "%"
}

func trimFloat(v float64) string {
	return strings.TrimSuffix(fmt.Sprintf("%.1f", v), ".0")
}

func elapsed(t Turn) string {
	d := t.UpdatedAt.Sub(t.StartedAt)
	if d < 0 {
		d = 0
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

// greyText renders secondary text without markup. A "<font>" tag cannot span
// a newline in Feishu markdown: the parser closes it at the end of the first
// line, and the trailing "</font>" then has no opening tag and shows up as
// literal text. Agent reasoning and shell commands are routinely multi-line,
// so anything that might contain one sets its colour as a property here
// instead of wrapping itself in markup. maxLines clamps natively; 0 leaves it
// unclamped.
func greyText(id, content, size string, maxLines int) map[string]any {
	text := map[string]any{
		"tag":        "plain_text",
		"content":    content,
		"text_size":  size,
		"text_color": "grey",
		"text_align": "left",
	}
	if maxLines > 0 {
		text["lines"] = maxLines
	}
	el := map[string]any{"tag": "div", "text": text}
	if id != "" {
		el["element_id"] = id
	}
	return el
}

func markdown(id, content, size string) map[string]any {
	el := map[string]any{
		"tag":        "markdown",
		"content":    content,
		"text_align": "left",
		"text_size":  size,
	}
	if id != "" {
		el["element_id"] = id
	}
	return el
}

// escape neutralises the markup Feishu parses out of a card's text. The
// closing ">" matters as much as the opening "<": leave a bare ">" in place
// and Feishu's parser desyncs on it, so the "</font>" that follows loses its
// opening tag and renders as literal text. Agent output is full of bare ">"
// — shell redirects, arrows, quoted lines — so this is not a rare case.
func escape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// escapeText additionally neutralises the markdown a card would otherwise
// apply to agent-supplied strings: a path like foo_bar_baz turns into italics
// and a glob like **/*.go turns into bold without it.
func escapeText(s string) string {
	s = escape(s)
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "*", "\\*")
	s = strings.ReplaceAll(s, "_", "\\_")
	return s
}

func truncateRunes(s string, max int) string {
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max]) + "…"
}

func shrinkRunes(s string, max int) string {
	if max < 1 {
		return ""
	}
	return truncateRunes(s, max)
}

func mustJSON(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		return []byte(`{"schema":"2.0","body":{"elements":[]}}`)
	}
	return raw
}

// boundQuestion trims an agent's question to what a card can hold. Feishu
// lays buttons out in one row, so a question with more choices than fit is
// dropped entirely rather than shown with some of its answers missing —
// silently hiding an option would put a choice the user never saw beyond
// their reach.
func boundQuestion(q *Question) *Question {
	if q == nil || q.RequestID == "" {
		return nil
	}
	if len(q.Choices) == 0 || len(q.Choices) > maxChoices {
		return nil
	}
	out := *q
	out.Message = truncateRunes(out.Message, maxQuestionRunes)
	out.Title = truncateRunes(out.Title, maxFieldLabel)
	out.Choices = make([]Choice, 0, len(q.Choices))
	for _, choice := range q.Choices {
		choice.Label = truncateRunes(choice.Label, maxChoiceLabel)
		choice.Detail = truncateRunes(choice.Detail, maxApprovalReason)
		out.Choices = append(out.Choices, choice)
	}
	return &out
}

// questionBlocks draws the agent's question with one button per answer. It
// reuses the approval container's look because it is the same interaction:
// the turn is parked until someone taps.
func questionBlocks(t Turn, copy Copy) []map[string]any {
	if t.Question == nil {
		return nil
	}
	title := copy.QuestionTitle
	if title == "" {
		title = "The agent has a question"
	}
	lines := []string{"**" + escape(title) + "**"}
	if t.Question.Message != "" {
		lines = append(lines, escape(t.Question.Message))
	}
	if t.Question.Title != "" {
		lines = append(lines, "**"+escape(t.Question.Title)+"**")
	}
	for _, choice := range t.Question.Choices {
		if choice.Detail == "" {
			continue
		}
		lines = append(lines, escape(choice.Label)+": "+escape(choice.Detail))
	}
	body := []map[string]any{markdown("", strings.Join(lines, "\n"), "normal")}
	if hint := copy.QuestionHint; hint != "" {
		body = append(body, greyText("", hint, "notation", 0))
	}
	columns := make([]map[string]any, 0, len(t.Question.Choices))
	for i, choice := range t.Question.Choices {
		style := "default"
		if i == 0 {
			style = "primary"
		}
		columns = append(columns, choiceButton(
			fmt.Sprintf("choice%d", i), choice.Label, style, t.Question.RequestID, choice.Value))
	}
	return []map[string]any{
		{
			"tag":              "interactive_container",
			"element_id":       "question",
			"width":            "fill",
			"has_border":       true,
			"border_color":     "blue-100",
			"corner_radius":    "8px",
			"background_style": "blue-50",
			"padding":          "12px 12px 12px 12px",
			"elements":         body,
		},
		{
			"tag":                "column_set",
			"element_id":         "question_buttons",
			"flex_mode":          "none",
			"horizontal_spacing": "8px",
			"columns":            columns,
		},
	}
}

// choiceButton carries the chosen value in the same "decision" slot the
// approval buttons use, so the channel's action parser needs no new field.
func choiceButton(id, label, style, requestID, value string) map[string]any {
	return map[string]any{
		"tag":            "column",
		"width":          "weighted",
		"weight":         1,
		"vertical_align": "center",
		"elements": []map[string]any{{
			"tag":   "button",
			"name":  id,
			"text":  map[string]any{"tag": "plain_text", "content": label},
			"type":  style,
			"width": "fill",
			"size":  "medium",
			"behaviors": []map[string]any{{
				"type": "callback",
				"value": map[string]any{
					"action":     cardActionQuestion,
					"request_id": requestID,
					"decision":   value,
				},
			}},
		}},
	}
}
