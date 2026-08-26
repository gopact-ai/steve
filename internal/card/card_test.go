package card

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func testCopy() Copy {
	return Copy{
		Title:        "Steve",
		Running:      "进行中",
		Completed:    "完成",
		Failed:       "失败",
		Cancelled:    "已取消",
		EarlierTools: "条更早的工具",
		Execution:    "执行过程",
		Input:        "输入",
		Output:       "输出",
		Context:      "Ctx",
		In:           "In",
		Out:          "Out",
		Hit:          "Hit",
		Write:        "Wr",
	}
}

func TestRenderReplyHasNoHeader(t *testing.T) {
	raw := Render(Turn{
		Status:    StatusRunning,
		Answer:    "hello <world>",
		Tools:     []Tool{{ID: "1", Name: "read", Status: ToolRunning}},
		StartedAt: time.Unix(0, 0),
		UpdatedAt: time.Unix(2, 0),
	}, testCopy())
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["schema"] != "2.0" {
		t.Fatalf("schema = %v", payload["schema"])
	}
	cfg := payload["config"].(map[string]any)
	if cfg["update_multi"] != true {
		t.Fatal("update_multi missing")
	}
	if _, ok := payload["header"]; ok {
		t.Fatalf("a plain reply should carry no header: %s", raw)
	}
	body := string(raw)
	if !strings.Contains(body, "⏳") || !strings.Contains(body, "read") {
		t.Fatalf("tools missing: %s", body)
	}
	if !strings.Contains(body, "hello") || !strings.Contains(body, "lt;world") {
		t.Fatalf("answer not escaped: %s", body)
	}
	if !strings.Contains(body, "进行中") || !strings.Contains(body, "2.0s") {
		t.Fatalf("status/elapsed missing: %s", body)
	}
	if !strings.Contains(body, "collapsible_panel") || !strings.Contains(body, "执行过程") {
		t.Fatalf("execution panel missing: %s", body)
	}
	if !strings.Contains(body, `"element_id":"meta"`) {
		t.Fatal("footer missing")
	}
}

func TestRenderFailedHighlightsErrorAndTitledCardKeepsHeader(t *testing.T) {
	raw := Render(Turn{
		Status: StatusFailed, Error: "boom",
		StartedAt: time.Unix(0, 0), UpdatedAt: time.Unix(1, 0),
	}, testCopy())
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if _, ok := payload["header"]; ok {
		t.Fatalf("a failed reply should carry no header: %s", raw)
	}

	titled := Render(Turn{
		Title: "状态", Status: StatusFailed, Error: "boom",
		StartedAt: time.Unix(0, 0), UpdatedAt: time.Unix(1, 0),
	}, testCopy())
	if err := json.Unmarshal(titled, &payload); err != nil {
		t.Fatal(err)
	}
	header := payload["header"].(map[string]any)
	if header["title"].(map[string]any)["content"] != "状态" {
		t.Fatalf("titled card lost its header: %s", titled)
	}
	if header["template"] != "red" {
		t.Fatalf("failed template = %v", header["template"])
	}
	body := string(raw)
	if !strings.Contains(body, `"background_style":"red-50"`) || !strings.Contains(body, "boom") {
		t.Fatalf("error block missing: %s", body)
	}
}

func TestRenderStatusUsesMetricCards(t *testing.T) {
	raw := Render(Turn{
		Title:  "状态",
		Status: StatusCompleted,
		Fields: []Field{
			{Label: "Agent", Value: "grok", IsMetric: true},
			{Label: "Mode", Value: "owner", IsMetric: true},
			{Label: "Home", Value: "/tmp/home", Wide: true},
		},
		StartedAt: time.Unix(0, 0),
		UpdatedAt: time.Unix(1, 0),
	}, testCopy())
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	cfg := payload["config"].(map[string]any)
	if cfg["width_mode"] != "compact" {
		t.Fatalf("status width = %v", cfg["width_mode"])
	}
	header := payload["header"].(map[string]any)
	if header["title"].(map[string]any)["content"] != "状态" {
		t.Fatalf("title = %#v", header["title"])
	}
	if header["subtitle"].(map[string]any)["content"] != "grok" {
		t.Fatalf("subtitle = %#v", header["subtitle"])
	}
	if !strings.Contains(string(raw), "1.0s") {
		t.Fatal("elapsed should sit in the footer")
	}
	body := payload["body"].(map[string]any)
	elements := body["elements"].([]any)
	if len(elements) != 3 {
		t.Fatalf("elements = %#v", elements)
	}
	if elements[0].(map[string]any)["tag"] != "column_set" {
		t.Fatalf("metrics missing: %#v", elements[0])
	}
	if elements[1].(map[string]any)["tag"] != "interactive_container" {
		t.Fatalf("details missing: %#v", elements[1])
	}
	if elements[2].(map[string]any)["element_id"] != "meta" {
		t.Fatalf("footer missing: %#v", elements[2])
	}
	rawStr := string(raw)
	if !strings.Contains(rawStr, "color='green'") || !strings.Contains(rawStr, "grok") {
		t.Fatalf("metric value missing: %s", rawStr)
	}
	if !strings.Contains(rawStr, `"background_style":"green-50"`) {
		t.Fatalf("metric fill missing: %s", rawStr)
	}
	if strings.Contains(rawStr, "**Agent**  grok") {
		t.Fatal("status should not render as a markdown list")
	}
}

func TestRenderToolPanelNestsInputOutput(t *testing.T) {
	raw := Render(Turn{
		Status: StatusRunning,
		Tools: []Tool{{
			ID: "1", Name: "read", Status: ToolRunning,
			Input: `{"path":"README.md"}`, Output: "# hi",
			Children: []Tool{{
				ID: "2", Name: "parse", Status: ToolCompleted,
				Input: "chunk", Output: "ok",
			}},
			StartedAt: time.Unix(0, 0), UpdatedAt: time.Unix(1, 0),
		}},
		StartedAt: time.Unix(0, 0),
		UpdatedAt: time.Unix(2, 0),
		Usage:     Usage{InputTokens: 1200, OutputTokens: 80, CacheReadTokens: 400, ContextTokens: 1600, ContextWindow: 128000},
	}, testCopy())
	body := string(raw)
	if !strings.Contains(body, "输入") || !strings.Contains(body, "README.md") {
		t.Fatalf("input missing: %s", body)
	}
	if !strings.Contains(body, "输出") || !strings.Contains(body, "# hi") {
		t.Fatalf("output missing: %s", body)
	}
	if !strings.Contains(body, "parse") || !strings.Contains(body, `"element_id":"t0c0"`) {
		t.Fatalf("nested tool missing: %s", body)
	}
	if !strings.Contains(body, "In 1.2K") || !strings.Contains(body, "Hit 400") || !strings.Contains(body, "Out 80") {
		t.Fatalf("usage footer missing: %s", body)
	}
}

func TestRenderExecutionShowsReasoning(t *testing.T) {
	raw := Render(Turn{
		Status:    StatusCompleted,
		Reasoning: "先读文件\n再改卡片",
		StartedAt: time.Unix(0, 0),
		UpdatedAt: time.Unix(1, 0),
	}, testCopy())
	body := string(raw)
	if !strings.Contains(body, "collapsible_panel") || !strings.Contains(body, "先读文件") {
		t.Fatalf("reasoning missing: %s", body)
	}
}

func TestFormatMarkdownKeepsBreaksAndLists(t *testing.T) {
	got := formatMarkdown("# 标题\n第一行\n第二行\n\n- a\n- b\n```\ncode\nline\n```")
	if !strings.Contains(got, "**标题**") {
		t.Fatalf("heading not compacted: %s", got)
	}
	if !strings.Contains(got, "第一行<br>\n第二行") {
		t.Fatalf("paragraphs not broken: %s", got)
	}
	if !strings.Contains(got, "- a\n- b") {
		t.Fatalf("list broken: %s", got)
	}
	if !strings.Contains(got, "```\ncode\nline\n```") {
		t.Fatalf("fence rewritten: %s", got)
	}
}

func TestRenderWakingPhaseIsDistinct(t *testing.T) {
	turn := Turn{Status: StatusRunning, StartedAt: time.Unix(0, 0), UpdatedAt: time.Unix(3, 0)}
	copy := testCopy()
	copy.Waking = "唤醒中"

	turn.Phase = PhaseWaking
	waking := string(Render(turn, copy))
	if !strings.Contains(waking, "唤醒中") {
		t.Fatalf("waking label missing: %s", waking)
	}

	turn.Phase = PhaseRunning
	running := string(Render(turn, copy))
	if strings.Contains(running, "唤醒中") {
		t.Fatalf("running should not claim waking: %s", running)
	}
	if !strings.Contains(running, "进行中") {
		t.Fatalf("running label missing: %s", running)
	}
}

func TestRenderControlSwitchesStopAndRetry(t *testing.T) {
	copy := testCopy()
	copy.Stop, copy.Retry = "终止", "重试"
	base := Turn{TurnID: "t1", StartedAt: time.Unix(0, 0), UpdatedAt: time.Unix(1, 0)}

	base.Status = StatusRunning
	running := string(Render(base, copy))
	if !strings.Contains(running, "终止") || !strings.Contains(running, `"action":"turn_cancel"`) {
		t.Fatalf("stop button missing: %s", running)
	}
	if strings.Contains(running, "重试") {
		t.Fatalf("running should not offer retry: %s", running)
	}

	base.Status, base.Error = StatusFailed, "boom"
	failed := string(Render(base, copy))
	if !strings.Contains(failed, "重试") || !strings.Contains(failed, `"action":"turn_retry"`) {
		t.Fatalf("retry button missing: %s", failed)
	}
	if !strings.Contains(failed, `"request_id":"t1"`) {
		t.Fatalf("turn id missing: %s", failed)
	}

	base.Status, base.Error = StatusCompleted, ""
	done := string(Render(base, copy))
	if strings.Contains(done, "turn_cancel") || strings.Contains(done, "turn_retry") {
		t.Fatalf("completed turn should have no control: %s", done)
	}
}

func TestRenderApprovalHidesControl(t *testing.T) {
	copy := testCopy()
	copy.Stop = "终止"
	raw := string(Render(Turn{
		Status: StatusRunning, TurnID: "t1",
		Approval:  &Approval{RequestID: "req_1", ToolName: "Write"},
		StartedAt: time.Unix(0, 0), UpdatedAt: time.Unix(1, 0),
	}, copy))
	if strings.Contains(raw, "turn_cancel") {
		t.Fatalf("approval should take over the card: %s", raw)
	}
}

func TestRenderApprovalShowsButtons(t *testing.T) {
	raw := Render(Turn{
		Status: StatusRunning,
		Approval: &Approval{
			RequestID: "req_1",
			ToolName:  "Write",
			Reason:    "/tmp/out.go",
		},
		StartedAt: time.Unix(0, 0),
		UpdatedAt: time.Unix(1, 0),
	}, testCopy())
	body := string(raw)
	if !strings.Contains(body, "需要授权") || !strings.Contains(body, "Write") || !strings.Contains(body, "/tmp/out.go") {
		t.Fatalf("approval text missing: %s", body)
	}
	if !strings.Contains(body, "允许一次") || !strings.Contains(body, "拒绝") {
		t.Fatalf("approval buttons missing: %s", body)
	}
	if !strings.Contains(body, `"request_id":"req_1"`) || !strings.Contains(body, `"action":"tool_approval"`) {
		t.Fatalf("callback payload missing: %s", body)
	}
	if !strings.Contains(body, `"background_style":"orange-50"`) {
		t.Fatalf("approval block should stand out: %s", body)
	}
}

func TestToolRowNamesCallAndKeepsArgumentsInside(t *testing.T) {
	raw := Render(Turn{
		Status: StatusRunning,
		Tools: []Tool{{
			ID: "1", Kind: "edit", Name: "Write file", Detail: "/tmp/out.go",
			Input: `{"path":"/tmp/out.go"}`, Status: ToolCompleted,
			StartedAt: time.Unix(0, 0), UpdatedAt: time.Unix(1, 0),
		}},
		StartedAt: time.Unix(0, 0),
		UpdatedAt: time.Unix(1, 0),
	}, testCopy())
	body := string(raw)
	if !strings.Contains(body, `"content":"✓ **Write file** `) || !strings.Contains(body, "· 1.0s") {
		t.Fatalf("collapsed row should name the call and show its duration: %s", body)
	}
	if !strings.Contains(body, "edit · /tmp/out.go") {
		t.Fatalf("kind and path belong inside the panel: %s", body)
	}
	if !strings.Contains(body, "输入") || !strings.Contains(body, `/tmp/out.go\"}`) {
		t.Fatalf("arguments should sit with the input: %s", body)
	}
}

func TestToolTitleDropsEmbeddedArguments(t *testing.T) {
	tests := []struct {
		name     string
		tool     Tool
		expected string
	}{
		{
			name:     "absolute path in title",
			tool:     Tool{Name: "Read file /Users/me/work/probe.txt", Detail: "/Users/me/work/probe.txt", Kind: "read"},
			expected: "Read file",
		},
		{
			name:     "title uses the base name",
			tool:     Tool{Name: "Edit probe.txt", Detail: "/Users/me/work/probe.txt", Kind: "edit"},
			expected: "Edit",
		},
		{
			name:     "path without a matching detail",
			tool:     Tool{Name: "Read file /Users/me/work/probe.txt", Kind: "read"},
			expected: "Read file",
		},
		{
			name:     "plain title survives",
			tool:     Tool{Name: "List files", Detail: "/Users/me/work", Kind: "read"},
			expected: "List files",
		},
		{
			name:     "falls back to kind",
			tool:     Tool{Name: "/Users/me/work/probe.txt", Detail: "/Users/me/work/probe.txt", Kind: "read"},
			expected: "read",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := toolTitle(tt.tool); got != tt.expected {
				t.Fatalf("title = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestRenderFitsByteCap(t *testing.T) {
	tools := make([]Tool, 20)
	for i := range tools {
		tools[i] = Tool{ID: "id", Name: strings.Repeat("n", 80), Detail: strings.Repeat("d", 240), Status: ToolCompleted}
	}
	raw := Render(Turn{
		Status:    StatusCompleted,
		Answer:    strings.Repeat("答", 20000),
		Tools:     tools,
		Error:     strings.Repeat("e", 2000),
		StartedAt: time.Unix(0, 0),
		UpdatedAt: time.Unix(1, 0),
	}, testCopy())
	if len(raw) > MaxBytes {
		t.Fatalf("card is %d bytes, cap %d", len(raw), MaxBytes)
	}
	if !utf8.Valid(raw) {
		t.Fatal("card JSON is invalid UTF-8")
	}
}
