// Package card renders one Feishu Card 2.0 for a Steve turn.
package card

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	StatusRunning   Status = "running"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"

	ToolRunning   ToolStatus = "running"
	ToolCompleted ToolStatus = "completed"
	ToolFailed    ToolStatus = "failed"
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
	maxReasoningRunes  = 360
	maxReasoningLines  = 6
	maxFieldLabel      = 40
	maxFieldValue      = 240
	maxApprovalReason  = 240
	headerIconToken    = "myai_colorful"
	cardActionApproval = "tool_approval"
	cardActionCancel   = "turn_cancel"
	cardActionRetry    = "turn_retry"
)

type Status string
type ToolStatus string

// Phase distinguishes "the agent process is still starting" from "the agent
// is working", so a cold npx start does not look like silent thinking.
type Phase string

const (
	PhaseWaking  Phase = "waking"
	PhaseRunning Phase = "running"
)

type Tool struct {
	ID string
	// Kind is the short tool name shown on the collapsed row; Name carries
	// the agent's full call description and moves inside the panel.
	Kind      string
	Name      string
	Detail    string
	Input     string
	Output    string
	Status    ToolStatus
	Children  []Tool
	StartedAt time.Time
	UpdatedAt time.Time
}

type Usage struct {
	InputTokens      uint64
	OutputTokens     uint64
	CacheReadTokens  uint64
	CacheWriteTokens uint64
	ContextTokens    uint64
	ContextWindow    uint64
}

type Field struct {
	Label    string
	Value    string
	Wide     bool
	IsMetric bool
}

type Progress struct {
	Answer    string
	Reasoning string
	Tools     []Tool
	Usage     Usage
}

type Copy struct {
	Title          string
	Running        string
	Completed      string
	Failed         string
	Cancelled      string
	EarlierTools   string
	Execution      string
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
	AllowOnce      string
	Deny           string
	Waking         string
	Stop           string
	Retry          string
}

type Turn struct {
	Title     string
	Status    Status
	Answer    string
	Reasoning string
	Error     string
	Fields    []Field
	Tools     []Tool
	Usage     Usage
	Approval  *Approval
	Phase     Phase
	TurnID    string
	StartedAt time.Time
	UpdatedAt time.Time
}

type Approval struct {
	RequestID string
	ToolName  string
	Reason    string
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
	return t
}

func build(t Turn, copy Copy) map[string]any {
	elements := make([]map[string]any, 0, 4)
	elements = append(elements, fieldBlocks(t)...)
	if panel := executionPanel(t, copy); panel != nil {
		elements = append(elements, panel)
	}
	elements = append(elements, approvalBlocks(t, copy)...)
	if t.Answer != "" {
		elements = append(elements, markdown("answer", formatMarkdown(t.Answer), "normal"))
	}
	if t.Error != "" {
		elements = append(elements, highlight("error", "red-50", "red-100", escape(t.Error)))
	}
	if control := controlRow(t, copy); control != nil {
		elements = append(elements, control)
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

// header is reserved for turns that carry their own title, such as a status
// report. A plain answer reads better without a banner; its state lives in
// the footer line.
func header(t Turn, copy Copy) map[string]any {
	title := strings.TrimSpace(t.Title)
	if title == "" {
		return nil
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

func executionPanel(t Turn, copy Copy) map[string]any {
	tools, hidden := visibleTools(t.Tools)
	if len(tools) == 0 && t.Reasoning == "" && t.Status != StatusRunning {
		return nil
	}
	elements := make([]map[string]any, 0, len(tools)+3)
	if t.Reasoning != "" {
		elements = append(elements, markdown("think", "<font color='grey'>"+escape(t.Reasoning)+"</font>", "notation"))
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
	title := fmt.Sprintf("%s **%s**", toolMark(tool.Status), escape(name))
	if d := toolDuration(tool, now); d != "" {
		// Feishu panel headers hold a single text element, so the duration
		// trails the name instead of sitting flush right.
		title += fmt.Sprintf(" <font color='grey'>· %s</font>", d)
	}
	elements := make([]map[string]any, 0, 4+len(tool.Children))
	if meta := toolMeta(tool); meta != "" {
		elements = append(elements, markdown("", "<font color='grey'>"+escape(meta)+"</font>", "notation"))
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

// toolTitle strips the arguments agents fold into their call description, so
// the collapsed row reads "Read file" instead of repeating the path that
// already sits inside the panel.
func toolTitle(tool Tool) string {
	name := tool.Name
	if tool.Detail != "" {
		name = strings.ReplaceAll(name, tool.Detail, "")
		name = strings.ReplaceAll(name, path.Base(tool.Detail), "")
	}
	fields := strings.Fields(name)
	kept := fields[:0]
	for _, field := range fields {
		if len(field) > 3 && strings.Contains(field, "/") {
			continue
		}
		kept = append(kept, field)
	}
	name = strings.Trim(strings.Join(kept, " "), " ：:·-—,，\"'`")
	return firstNonEmpty(name, tool.Kind, tool.ID)
}

// toolMeta is the call's kind and the file it touched, kept inside the panel
// next to the input and output rather than crowding the collapsed row.
func toolMeta(tool Tool) string {
	parts := make([]string, 0, 2)
	if tool.Kind != "" && tool.Kind != tool.Name {
		parts = append(parts, tool.Kind)
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

func formatMarkdown(s string) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return ""
	}
	src := strings.Split(compactHeadings(s), "\n")
	out := make([]string, 0, len(src))
	inFence := false
	for i, line := range src {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			out = append(out, escape(line))
			continue
		}
		next := ""
		if i+1 < len(src) {
			next = src[i+1]
		}
		broken := !inFence && !isLooseLine(line) && strings.TrimSpace(next) != "" &&
			!isLooseLine(next) && !strings.HasPrefix(strings.TrimSpace(next), "```")
		if broken {
			out = append(out, escape(line)+"<br>")
			continue
		}
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

func isLooseLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || trimmed == "---" || trimmed == "***" {
		return true
	}
	if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") || strings.HasPrefix(trimmed, "> ") {
		return true
	}
	if len(trimmed) > 2 && trimmed[0] >= '1' && trimmed[0] <= '9' {
		dot := strings.IndexByte(trimmed, '.')
		if dot > 0 && dot+1 < len(trimmed) && trimmed[dot+1] == ' ' {
			return true
		}
	}
	return false
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
	lines = append(lines, "<font color='grey'>"+escape(rule)+"</font>")
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
			"elements":         []map[string]any{markdown("", strings.Join(lines, "\n"), "normal")},
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

func footerText(t Turn, copy Copy) string {
	label, _, _ := headerTone(t, copy)
	parts := []string{label, elapsed(t)}
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

func escape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	return s
}

func truncateRunes(s string, max int) string {
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max])
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
