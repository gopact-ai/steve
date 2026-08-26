package card

import (
	"encoding/json"
	"fmt"
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

func TestRunningReplyCarriesColoredHeader(t *testing.T) {
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
	// The header is the state made scannable: a running card is blue with a
	// 进行中 tag, readable from the chat list without opening it.
	header, ok := payload["header"].(map[string]any)
	if !ok {
		t.Fatalf("running reply lost its header: %s", raw)
	}
	if header["template"] != "blue" {
		t.Fatalf("running template = %v", header["template"])
	}
	if header["title"].(map[string]any)["content"] != "Steve" {
		t.Fatalf("untitled running card should borrow the product name: %s", raw)
	}
	// A finished plain answer is an archive with no state left to announce.
	done := Render(Turn{
		Status: StatusCompleted, Answer: "done",
		StartedAt: time.Unix(0, 0), UpdatedAt: time.Unix(2, 0),
	}, testCopy())
	var donePayload map[string]any
	if err := json.Unmarshal(done, &donePayload); err != nil {
		t.Fatal(err)
	}
	if _, ok := donePayload["header"]; ok {
		t.Fatalf("completed plain answer should drop the header: %s", done)
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
	if h, ok := payload["header"].(map[string]any); !ok || h["template"] != "red" {
		t.Fatalf("failed reply should carry a red header: %s", raw)
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
	// A bare newline is already a line break in Feishu, so adding "<br>" on
	// top of it renders a blank line under every wrapped line. Probed: "A\nB"
	// is two lines, "A<br>\nB" is two lines with a gap.
	if strings.Contains(got, "<br>") {
		t.Fatalf("a <br> beside a newline doubles the break: %s", got)
	}
	if !strings.Contains(got, "第一行\n第二行") {
		t.Fatalf("hard break lost: %s", got)
	}
	if !strings.Contains(got, "- a\n- b") {
		t.Fatalf("list broken: %s", got)
	}
	if !strings.Contains(got, "```\ncode\nline\n```") {
		t.Fatalf("fence rewritten: %s", got)
	}
	// A deliberate blank line is the author's, and stays exactly one.
	if strings.Contains(got, "\n\n\n") {
		t.Fatalf("blank lines multiplied: %q", got)
	}
}

// Every multi-line thing the card renders goes out with single newlines, so
// nothing arrives double-spaced.
func TestRenderedCardHasNoDoubledLineBreaks(t *testing.T) {
	raw := Render(Turn{
		Status:    StatusCompleted,
		Answer:    "当前模型：GPT 5.6 Sol\n可选：\n▸ GPT 5.6 Sol\nGPT 5.6 Terra\n用 /model <名称> 切换",
		Reasoning: "thinking one\nthinking two",
		Plan:      []Step{{Text: "one", Status: StepCompleted}, {Text: "two", Status: StepPending}},
		StartedAt: time.Unix(0, 0), UpdatedAt: time.Unix(1, 0),
	}, testCopy())
	if strings.Contains(string(raw), "<br>") {
		t.Fatalf("card still emits <br>: %s", raw)
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

func TestToolRowShowsKindAndKeepsTheCallInside(t *testing.T) {
	raw := Render(Turn{
		Status: StatusRunning,
		Tools: []Tool{{
			ID: "1", Kind: "execute", Name: "go test ./... 2>&1 | tail", Detail: "/tmp/out.go",
			Input: `{"path":"/tmp/out.go"}`, Status: ToolCompleted,
			StartedAt: time.Unix(0, 0), UpdatedAt: time.Unix(1, 0),
		}},
		StartedAt: time.Unix(0, 0),
		UpdatedAt: time.Unix(1, 0),
	}, testCopy())
	body := string(raw)
	// The row says what kind of call this is. ACP's title is the agent's own
	// description — for a shell call, the command — so it belongs in the panel.
	if !strings.Contains(body, `**execute**`) || !strings.Contains(body, "· 1.0s") {
		t.Fatalf("collapsed row should carry the kind and duration: %s", body)
	}
	if strings.Contains(body, `✓ ⌨️ **go test`) {
		t.Fatalf("the command must not be the row label: %s", body)
	}
	if !strings.Contains(body, "go test") || !strings.Contains(body, "/tmp/out.go") {
		t.Fatalf("the command and path belong inside the panel: %s", body)
	}
	if !strings.Contains(body, "输入") {
		t.Fatalf("arguments should sit with the input: %s", body)
	}
}

func TestToolTitlePrefersKind(t *testing.T) {
	tests := []struct {
		name     string
		tool     Tool
		expected string
	}{
		{
			name:     "kind wins over the agent's call description",
			tool:     Tool{Name: "Read file /Users/me/work/probe.txt", Detail: "/Users/me/work/probe.txt", Kind: "read"},
			expected: "read",
		},
		{
			name:     "a command never becomes the label",
			tool:     Tool{Name: "rg -n 'func main' .", Kind: "search"},
			expected: "search",
		},
		{
			name:     "falls back to the title when the agent sends no kind",
			tool:     Tool{Name: "List files", Detail: "/Users/me/work"},
			expected: "List files",
		},
		{
			name:     "falls back to the id when there is nothing else",
			tool:     Tool{ID: "call_7"},
			expected: "call_7",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := toolTitle(test.tool); got != test.expected {
				t.Fatalf("toolTitle = %q; want %q", got, test.expected)
			}
		})
	}
}

func TestEscapeClosesFeishuMarkup(t *testing.T) {
	// A bare ">" desyncs Feishu's parser, which orphans the "</font>" that
	// wraps the text and renders it as literal characters in the card.
	got := escape("go test ./... 2>&1 && echo <done>")
	for _, bare := range []string{"<", ">"} {
		if strings.Contains(got, bare) {
			t.Fatalf("escape left a bare %q: %q", bare, got)
		}
	}
	if !strings.Contains(got, "&gt;") || !strings.Contains(got, "&lt;") || !strings.Contains(got, "&amp;") {
		t.Fatalf("escape = %q", got)
	}
}

func TestRenderedCardHasNoOrphanFontTags(t *testing.T) {
	raw := string(Render(Turn{
		Status:    StatusRunning,
		Reasoning: "piping with 2>&1 then <check>",
		Answer:    "done > /dev/null",
		Tools: []Tool{{
			ID: "1", Kind: "execute", Name: "sh -c 'a > b'", Status: ToolRunning,
			StartedAt: time.Unix(0, 0), UpdatedAt: time.Unix(1, 0),
		}},
		StartedAt: time.Unix(0, 0),
		UpdatedAt: time.Unix(1, 0),
	}, testCopy()))
	// json.Marshal escapes "<" as \u003c, so count the escaped forms.
	if open, close := strings.Count(raw, `u003cfont`), strings.Count(raw, `u003c/font`); open != close {
		t.Fatalf("unbalanced font tags: %d open vs %d close", open, close)
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

func TestFooterLeadsWithHarnessAndModel(t *testing.T) {
	turn := Turn{
		Status:    StatusCompleted,
		Answer:    "done",
		Settings:  Settings{Harness: "codex", Model: "GPT 5.6 Sol", Mode: "Agent"},
		Usage:     Usage{ContextTokens: 11000, ContextWindow: 272000},
		StartedAt: time.Unix(0, 0),
		UpdatedAt: time.Unix(12, 0),
	}
	got := footerText(turn, testCopy())
	want := "codex · GPT 5.6 Sol · Agent · 完成 · 12.0s · Ctx 11K/272K (4%)"
	if got != want {
		t.Fatalf("footer = %q, want %q", got, want)
	}
	// The chat-list preview truncates the footer, so the settings have to
	// lead it to survive.
	if !strings.HasPrefix(summary(turn, testCopy()), "codex · GPT 5.6 Sol") {
		t.Fatalf("summary drops the settings: %q", summary(turn, testCopy()))
	}
}

func TestFooterOmitsUnreportedSettings(t *testing.T) {
	turn := Turn{
		Status:    StatusRunning,
		Settings:  Settings{Harness: "codex"},
		StartedAt: time.Unix(0, 0),
		UpdatedAt: time.Unix(1, 0),
	}
	if got, want := footerText(turn, testCopy()), "codex · 进行中 · 1.0s"; got != want {
		t.Fatalf("footer = %q, want %q", got, want)
	}
	bare := Turn{Status: StatusRunning, StartedAt: time.Unix(0, 0), UpdatedAt: time.Unix(1, 0)}
	if got, want := footerText(bare, testCopy()), "进行中 · 1.0s"; got != want {
		t.Fatalf("footer = %q, want %q", got, want)
	}
}

// A model name is agent-supplied text on a line that already uses "·" as a
// separator, so it must not be able to run away with the footer.
func TestFooterCapsSettingValues(t *testing.T) {
	long := strings.Repeat("m", 100)
	turn := Turn{
		Status:    StatusRunning,
		Settings:  Settings{Model: long},
		StartedAt: time.Unix(0, 0),
		UpdatedAt: time.Unix(1, 0),
	}
	got := footerText(turn, testCopy())
	if strings.Contains(got, long) {
		t.Fatalf("footer kept the full model name: %q", got)
	}
	if !strings.Contains(got, "…") {
		t.Fatalf("footer did not mark the model as trimmed: %q", got)
	}
}

func TestPlanRendersAboveExecution(t *testing.T) {
	copy := testCopy()
	copy.Plan = "计划"
	raw := Render(Turn{
		Status: StatusRunning,
		Plan: []Step{
			{Text: "look around", Status: StepCompleted},
			{Text: "do the thing", Status: StepInProgress},
			{Text: "check it", Status: StepPending},
		},
		Tools:     []Tool{{ID: "1", Kind: "read", Status: ToolRunning}},
		StartedAt: time.Unix(0, 0),
		UpdatedAt: time.Unix(1, 0),
	}, copy)
	body := string(raw)
	if !strings.Contains(body, "计划 1/3") {
		t.Fatalf("plan header missing or miscounted: %s", body)
	}
	for _, want := range []string{"✓ look around", "◉ do the thing", "○ check it"} {
		if !strings.Contains(body, want) {
			t.Fatalf("step %q missing: %s", want, body)
		}
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	elements := payload["body"].(map[string]any)["elements"].([]any)
	if elements[0].(map[string]any)["element_id"] != "plan" {
		t.Fatalf("plan should lead the body, got %v", elements[0])
	}
}

// A plan longer than the card can hold keeps its tail and says how much it
// dropped, rather than silently shortening the agent's plan.
func TestLongPlanReportsWhatItTrimmed(t *testing.T) {
	copy := testCopy()
	copy.Plan = "计划"
	copy.EarlierSteps = "个更早的步骤"
	steps := make([]Step, 0, maxVisibleSteps+3)
	for i := 0; i < maxVisibleSteps+3; i++ {
		steps = append(steps, Step{Text: fmt.Sprintf("step %d", i), Status: StepCompleted})
	}
	body := string(Render(Turn{
		Status: StatusRunning, Plan: steps,
		StartedAt: time.Unix(0, 0), UpdatedAt: time.Unix(1, 0),
	}, copy))
	if !strings.Contains(body, "3 个更早的步骤") {
		t.Fatalf("trim not reported: %s", body)
	}
	if !strings.Contains(body, fmt.Sprintf("计划 %d/%d", len(steps), len(steps))) {
		t.Fatalf("header should count the whole plan: %s", body)
	}
	if strings.Contains(body, "step 0") {
		t.Fatalf("oldest step should have been trimmed: %s", body)
	}
	if !strings.Contains(body, fmt.Sprintf("step %d", len(steps)-1)) {
		t.Fatalf("newest step should survive: %s", body)
	}
}

func TestPlanEscapesAgentText(t *testing.T) {
	body := string(Render(Turn{
		Status:    StatusRunning,
		Plan:      []Step{{Text: "grep foo > out.txt & run *all*", Status: StepPending}},
		StartedAt: time.Unix(0, 0), UpdatedAt: time.Unix(1, 0),
	}, testCopy()))
	if strings.Contains(body, "foo > out") {
		t.Fatalf("plan text not escaped: %s", body)
	}
	if !strings.Contains(body, "gt;") {
		t.Fatalf("expected an escaped angle bracket: %s", body)
	}
}

func TestNoPlanBlockWhenPlanEmpty(t *testing.T) {
	body := string(Render(Turn{
		Status: StatusRunning, StartedAt: time.Unix(0, 0), UpdatedAt: time.Unix(1, 0),
	}, testCopy()))
	if strings.Contains(body, `"element_id":"plan"`) {
		t.Fatalf("empty plan should render nothing: %s", body)
	}
}

// A "<font>" tag cannot span a newline in Feishu markdown — the parser closes
// it at the end of the first line and the trailing "</font>" then renders as
// literal text on the card. This walks every markdown element the renderer
// produces and fails if any of them opens a font tag in a block that also
// contains a newline.
func TestNoFontTagSpansANewline(t *testing.T) {
	copy := testCopy()
	copy.Plan = "计划"
	copy.EarlierSteps = "个更早的步骤"
	copy.ApprovalRule = "仅此一次"
	steps := make([]Step, 0, maxVisibleSteps+2)
	for i := 0; i < maxVisibleSteps+2; i++ {
		steps = append(steps, Step{Text: fmt.Sprintf("step %d", i), Status: StepPending})
	}
	turn := Turn{
		Title:     "Steve",
		Status:    StatusRunning,
		Answer:    "line one\nline two",
		Reasoning: "**Thinking hard**\nabout several\nseparate lines",
		Plan:      steps,
		Fields:    []Field{{Label: "agent", Value: "codex"}, {Label: "ctx", Value: "1K", IsMetric: true}},
		Tools: []Tool{{
			ID: "1", Kind: "execute", Name: "cd /tmp\nls -la\necho done",
			Input: "in", Output: "out", Status: ToolRunning,
		}},
		Settings:  Settings{Harness: "codex", Model: "GPT 5.6 Sol", Mode: "Agent"},
		Approval:  &Approval{RequestID: "r1", ToolName: "rm", Reason: "danger"},
		StartedAt: time.Unix(0, 0),
		UpdatedAt: time.Unix(3, 0),
	}
	var payload map[string]any
	if err := json.Unmarshal(Render(turn, copy), &payload); err != nil {
		t.Fatal(err)
	}
	var walk func(any, string)
	walk = func(node any, path string) {
		switch v := node.(type) {
		case map[string]any:
			if v["tag"] == "markdown" {
				content, _ := v["content"].(string)
				if strings.Contains(content, "<font") && strings.Contains(content, "\n") {
					t.Errorf("%s: font tag shares a markdown block with a newline:\n%q", path, content)
				}
			}
			for key, child := range v {
				walk(child, path+"."+key)
			}
		case []any:
			for i, child := range v {
				walk(child, fmt.Sprintf("%s[%d]", path, i))
			}
		}
	}
	walk(payload, "card")
}

// Reasoning is agent-written and routinely multi-line, so it must not be
// rendered as markup at all.
func TestReasoningRendersAsPlainText(t *testing.T) {
	raw := Render(Turn{
		Status:    StatusRunning,
		Reasoning: "**Locating dir**\nchecking > output",
		StartedAt: time.Unix(0, 0),
		UpdatedAt: time.Unix(1, 0),
	}, testCopy())
	body := string(raw)
	if strings.Contains(body, "</font>") && strings.Contains(body, "Locating") {
		t.Fatalf("reasoning still wrapped in font markup: %s", body)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	found := false
	var walk func(any)
	walk = func(node any) {
		switch v := node.(type) {
		case map[string]any:
			if v["element_id"] == "think" {
				text, ok := v["text"].(map[string]any)
				if !ok {
					t.Fatalf("reasoning element is not a text div: %v", v)
				}
				if text["tag"] != "plain_text" {
					t.Errorf("reasoning tag = %v, want plain_text", text["tag"])
				}
				if text["text_color"] != "grey" {
					t.Errorf("reasoning colour = %v, want grey", text["text_color"])
				}
				// Unparsed means the agent's own characters survive intact.
				if text["content"] != "**Locating dir**\nchecking > output" {
					t.Errorf("reasoning content mangled: %q", text["content"])
				}
				found = true
			}
			for _, child := range v {
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(payload)
	if !found {
		t.Fatal("no reasoning element rendered")
	}
}

func TestQuestionRendersOneButtonPerChoice(t *testing.T) {
	copy := testCopy()
	copy.QuestionTitle = "需要你确认"
	raw := Render(Turn{
		Status: StatusRunning,
		Question: &Question{
			RequestID: "r1",
			Message:   "Which colour do you prefer?",
			Title:     "Colour",
			Choices: []Choice{
				{Value: "Red", Label: "Red", Detail: "You prefer red."},
				{Value: "Blue", Label: "Blue"},
			},
		},
		StartedAt: time.Unix(0, 0), UpdatedAt: time.Unix(1, 0),
	}, copy)
	body := string(raw)
	for _, want := range []string{"需要你确认", "Which colour", "elicit_answer", `"decision":"Red"`, `"decision":"Blue"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q: %s", want, body)
		}
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	buttons := 0
	var walk func(any)
	walk = func(node any) {
		switch v := node.(type) {
		case map[string]any:
			if v["tag"] == "button" {
				buttons++
			}
			for _, child := range v {
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(payload)
	if buttons != 2 {
		t.Fatalf("buttons = %d, want one per choice", buttons)
	}
}

// Feishu lays the answers out in one row, so a question with more choices
// than fit is dropped whole rather than shown with answers missing.
func TestQuestionWithTooManyChoicesIsDropped(t *testing.T) {
	choices := make([]Choice, 0, maxChoices+1)
	for i := 0; i <= maxChoices; i++ {
		choices = append(choices, Choice{Value: fmt.Sprint(i), Label: fmt.Sprint(i)})
	}
	body := string(Render(Turn{
		Status:    StatusRunning,
		Question:  &Question{RequestID: "r1", Message: "pick", Choices: choices},
		StartedAt: time.Unix(0, 0), UpdatedAt: time.Unix(1, 0),
	}, testCopy()))
	if strings.Contains(body, "elicit_answer") {
		t.Fatalf("oversized question should render nothing: %s", body)
	}
}

func TestQuestionWithoutRequestIDIsDropped(t *testing.T) {
	body := string(Render(Turn{
		Status:    StatusRunning,
		Question:  &Question{Message: "pick", Choices: []Choice{{Value: "a", Label: "a"}}},
		StartedAt: time.Unix(0, 0), UpdatedAt: time.Unix(1, 0),
	}, testCopy()))
	if strings.Contains(body, "elicit_answer") {
		t.Fatalf("unanswerable question should render nothing: %s", body)
	}
}
