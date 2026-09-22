package card

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func decodeCard(t *testing.T, turn Turn) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(Render(turn, testCopy()), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func cardElement(t *testing.T, payload map[string]any, id string) map[string]any {
	t.Helper()
	for _, el := range payload["body"].(map[string]any)["elements"].([]any) {
		m := el.(map[string]any)
		if m["element_id"] == id {
			return m
		}
	}
	t.Fatalf("missing element %s", id)
	return nil
}

func TestConversationStateLivesInBody(t *testing.T) {
	for _, status := range []Status{StatusRunning, StatusCompleted, StatusFailed, StatusCancelled} {
		t.Run(string(status), func(t *testing.T) {
			p := decodeCard(t, Turn{Status: status, Answer: "answer", Error: "error", TurnID: "turn"})
			if _, ok := p["header"]; ok {
				t.Fatal("conversation title banner returned")
			}
			if status == StatusRunning {
				cardElement(t, p, "activity")
			}
		})
	}
	waking := decodeCard(t, Turn{Status: StatusRunning, Phase: PhaseWaking})
	activity := cardElement(t, waking, "activity")
	if !strings.Contains(activity["text"].(map[string]any)["content"].(string), "唤醒") {
		t.Fatalf("waking state invisible: %#v", activity)
	}
}

func TestConversationShowsCurrentActionAndKeepsAnswerBeforeDetails(t *testing.T) {
	p := decodeCard(t, Turn{Status: StatusRunning, Answer: "partial answer", Reasoning: "inspecting", Tools: []Tool{{ID: "1", Kind: "read", Name: "read config.go", Status: ToolRunning}}})
	if got := cardElement(t, p, "activity")["text"].(map[string]any)["content"].(string); !strings.Contains(got, "read config.go") {
		t.Fatalf("action missing: %s", got)
	}
	elements := p["body"].(map[string]any)["elements"].([]any)
	answerIndex, execIndex := -1, -1
	for i, el := range elements {
		switch el.(map[string]any)["element_id"] {
		case "answer":
			answerIndex = i
		case "exec":
			execIndex = i
		}
	}
	if answerIndex < 0 || execIndex <= answerIndex {
		t.Fatal("answer must precede execution details")
	}
	if cardElement(t, p, "exec")["expanded"] != true {
		t.Fatal("live details should be visible")
	}
	done := decodeCard(t, Turn{Status: StatusCompleted, Answer: "done", Tools: []Tool{{ID: "1", Name: "read", Status: ToolCompleted}}})
	if cardElement(t, done, "exec")["expanded"] != false {
		t.Fatal("finished details should fold away")
	}
}

func TestConversationStopIsSecondary(t *testing.T) {
	p := decodeCard(t, Turn{Status: StatusRunning, TurnID: "turn"})
	control := cardElement(t, p, "control")
	button := control["columns"].([]any)[0].(map[string]any)["elements"].([]any)[0].(map[string]any)
	if button["width"] == "fill" || button["type"] == "danger" {
		t.Fatalf("stop still dominates card: %#v", button)
	}
	value := button["behaviors"].([]any)[0].(map[string]any)["value"].(map[string]any)
	if value["action"] != "turn_cancel" || value["request_id"] != "turn" {
		t.Fatal("cancel callback changed")
	}
}

func TestRenderDoesNotMutateProgress(t *testing.T) {
	original := Turn{Status: StatusRunning, Tools: []Tool{{Name: strings.Repeat("n", 500), Children: []Tool{{Output: strings.Repeat("o", 1000)}}}}, Fields: []Field{{Value: strings.Repeat("v", 400)}}, Approval: &Approval{RequestID: "request", Reason: strings.Repeat("r", 500)}}
	raw, _ := json.Marshal(original)
	var expected Turn
	json.Unmarshal(raw, &expected)
	Render(original, testCopy())
	if !reflect.DeepEqual(original, expected) {
		t.Fatal("render mutated shared progress snapshot")
	}
}

func TestExecutionDetailsStayWithinPayloadBudget(t *testing.T) {
	tools := make([]Tool, 12)
	for i := range tools {
		tools[i] = Tool{ID: "tool", Input: strings.Repeat("<&", 800), Output: strings.Repeat("<&", 800), Status: ToolRunning}
	}
	tools[len(tools)-1].Children = append([]Tool(nil), tools[1:]...)
	raw := Render(Turn{Status: StatusRunning, Answer: "important answer", Reasoning: strings.Repeat("思", 2000), Tools: tools, StartedAt: time.Unix(1, 0)}, testCopy())
	if len(raw) > MaxBytes {
		t.Fatalf("oversized card: %d", len(raw))
	}
	if !strings.Contains(string(raw), "important answer") {
		t.Fatal("details displaced answer")
	}
}

func TestExecutionTreeIsBoundedAndKeepsRecentRootTools(t *testing.T) {
	deep := Tool{Name: "deep", Status: ToolRunning}
	for i := 0; i < 20; i++ {
		deep = Tool{Name: "parent", Status: ToolRunning, Children: []Tool{deep}}
	}
	tools := []Tool{deep, deep, deep, deep, deep, {Name: "latest tool", Status: ToolRunning}}
	p := decodeCard(t, Turn{Status: StatusRunning, Tools: tools})
	count, maxDepth := 0, 0
	var visit func(any, int)
	visit = func(value any, depth int) {
		switch v := value.(type) {
		case map[string]any:
			if v["tag"] == "collapsible_panel" {
				depth++
				count++
				if depth > maxDepth {
					maxDepth = depth
				}
			}
			for _, child := range v {
				visit(child, depth)
			}
		case []any:
			for _, child := range v {
				visit(child, depth)
			}
		}
	}
	visit(p, 0)
	if count > 13 || maxDepth > 4 {
		t.Fatalf("unbounded execution panels: count=%d depth=%d", count, maxDepth)
	}
	raw, _ := json.Marshal(p)
	if !strings.Contains(string(raw), "latest tool") {
		t.Fatal("older children displaced latest root tool")
	}
}

func TestExecutionReportsHiddenRootTools(t *testing.T) {
	tools := make([]Tool, maxVisibleTools+3)
	for i := range tools {
		tools[i] = Tool{Name: "read", Status: ToolCompleted}
	}
	raw := string(Render(Turn{Status: StatusCompleted, Tools: tools}, testCopy()))
	if !strings.Contains(raw, "3 条更早的工具") {
		t.Fatal("trimmed tools must be disclosed")
	}
}

func TestConversationWaitingDoesNotPretendToExecute(t *testing.T) {
	for _, turn := range []Turn{
		{Status: StatusRunning, Phase: PhaseWaking, Approval: &Approval{RequestID: "a", ToolName: "write"}},
		{Status: StatusRunning, Question: &Question{RequestID: "q", Message: "pick", Choices: []Choice{{Value: "yes", Label: "yes"}}}},
	} {
		turn.Tools = []Tool{{Name: "should not be current action", Status: ToolRunning}}
		p := decodeCard(t, turn)
		activity := cardElement(t, p, "activity")["text"].(map[string]any)["content"].(string)
		if strings.Contains(activity, "should not") || strings.Contains(activity, "唤醒") {
			t.Fatalf("wrong waiting status: %s", activity)
		}
	}
}

func TestFooterShowsOnlyReportedUsage(t *testing.T) {
	if got := footerText(Turn{Status: StatusRunning}, testCopy()); strings.Contains(got, "Ctx") || strings.Contains(got, "In") || strings.Contains(got, "0.0s") {
		t.Fatalf("invented telemetry: %s", got)
	}
	got := footerText(Turn{Status: StatusCompleted, Usage: Usage{Reported: true, TotalTokens: 1200}}, testCopy())
	if !strings.Contains(got, "Tokens 1.2K") || strings.Contains(got, "In 0") {
		t.Fatalf("total-only usage must not fabricate a breakdown: %s", got)
	}
	got = footerText(Turn{Usage: Usage{CacheReadTokens: 4000, CacheWriteTokens: 1000}}, testCopy())
	if !strings.Contains(got, "Hit 4K") || !strings.Contains(got, "Wr 1K") {
		t.Fatalf("cache counters missing: %s", got)
	}
}

func TestBudgetShrinkingMakesProgress(t *testing.T) {
	for _, text := range []string{"…", "a…", "abc…"} {
		got := shrinkRunes(text, len([]rune(text))*3/4)
		if len([]rune(got)) >= len([]rune(text)) {
			t.Fatalf("budget shrink does not converge: %q -> %q", text, got)
		}
	}
}

func TestEscapedPlanCannotExceedCardBudget(t *testing.T) {
	steps := make([]Step, maxVisibleSteps)
	for i := range steps {
		steps[i] = Step{Text: strings.Repeat("<>&", maxStepRunes/3), Status: StepInProgress}
	}
	raw := Render(Turn{Status: StatusRunning, Answer: "answer survives", Plan: steps}, testCopy())
	if len(raw) > MaxBytes {
		t.Fatalf("escaped plan exceeds payload limit: %d", len(raw))
	}
	if !strings.Contains(string(raw), "answer survives") {
		t.Fatal("plan displaced the answer")
	}
}

func TestTrimmedChildToolsAreDisclosed(t *testing.T) {
	children := make([]Tool, maxVisibleTools+1)
	for i := range children {
		children[i] = Tool{Name: "child", Status: ToolCompleted}
	}
	raw := string(Render(Turn{Status: StatusCompleted, Tools: []Tool{{Name: "parent", Children: children}}}, testCopy()))
	if !strings.Contains(raw, "部分详情已省略") {
		t.Fatal("child tools silently omitted")
	}
}

func TestOversizedFieldsDoNotDisplaceAnswer(t *testing.T) {
	fields := make([]Field, 100)
	for i := range fields {
		fields[i] = Field{Label: strings.Repeat("&", 40), Value: strings.Repeat("<>&", 80), IsMetric: i%2 == 0}
	}
	raw := Render(Turn{Status: StatusCompleted, Answer: "answer survives", Fields: fields}, testCopy())
	if len(raw) > MaxBytes {
		t.Fatalf("oversized field card: %d", len(raw))
	}
	if !strings.Contains(string(raw), "answer survives") || !strings.Contains(string(raw), "部分详情已省略") {
		t.Fatal("answer or omission notice missing")
	}
}
