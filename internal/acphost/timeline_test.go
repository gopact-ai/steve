package acphost

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/view"
)

func TestTimelineMergesChunksAndKeepsToolCreationOrder(t *testing.T) {
	var snapshots []view.Progress
	c := &collector{progress: func(p view.Progress) { snapshots = append(snapshots, p) }}
	chunk := func(kind acp.SessionUpdateType, text string) {
		c.handle(acp.SessionUpdate{SessionUpdate: kind, Content: acp.TextContentBlock(text)})
	}
	chunk(acp.SessionUpdateTypeAgentMessageChunk, "先看")
	chunk(acp.SessionUpdateTypeAgentMessageChunk, "项目。")
	first := snapshots[len(snapshots)-1]
	chunk(acp.SessionUpdateTypeAgentThoughtChunk, "检查")
	chunk(acp.SessionUpdateTypeAgentThoughtChunk, "结构。")
	c.handle(acp.SessionUpdate{SessionUpdate: acp.SessionUpdateTypeToolCall, ToolCallID: "read-1"})
	c.handle(acp.SessionUpdate{SessionUpdate: acp.SessionUpdateTypeToolCall, ToolCallID: "run-1"})
	chunk(acp.SessionUpdateTypeAgentMessageChunk, "看到了结果。")
	done := acp.ToolCallStatusCompleted
	c.handle(acp.SessionUpdate{SessionUpdate: acp.SessionUpdateTypeToolCallUpdate, ToolCallID: "read-1", Status: &done, RawOutput: "contents"})
	// Replayed creation and late status updates cannot split narration.
	c.handle(acp.SessionUpdate{SessionUpdate: acp.SessionUpdateTypeToolCall, ToolCallID: "run-1"})
	chunk(acp.SessionUpdateTypeAgentMessageChunk, "继续修改。")
	chunk(acp.SessionUpdateTypeAgentThoughtChunk, "验证。")
	chunk(acp.SessionUpdateTypeAgentMessageChunk, "完成。")
	chunk(acp.SessionUpdateTypeAgentThoughtChunk, "")
	chunk(acp.SessionUpdateTypeUserMessageChunk, "不要重放用户的话")
	got, _ := c.snapshot()
	want := []view.Span{
		{Kind: "text", Text: "先看项目。"}, {Kind: "thought", Text: "检查结构。"},
		{Kind: "tool", Tool: "read-1"}, {Kind: "tool", Tool: "run-1"},
		{Kind: "text", Text: "看到了结果。继续修改。"}, {Kind: "thought", Text: "验证。"},
		{Kind: "text", Text: "完成。"},
	}
	if len(got.Timeline) != len(want) {
		t.Fatalf("timeline = %+v", got.Timeline)
	}
	for i, span := range got.Timeline {
		if span.At.IsZero() || (i > 0 && span.At.Before(got.Timeline[i-1].At)) {
			t.Fatalf("span %d lost its arrival timestamp", i)
		}
		want[i].At = span.At
	}
	if !reflect.DeepEqual(got.Timeline, want) || got.Answer != "先看项目。看到了结果。继续修改。完成。" || got.Reasoning != "检查结构。验证。" || got.Tools[0].Status != view.ToolCompleted {
		t.Fatalf("timeline or legacy projections changed: %+v", got)
	}
	if len(first.Timeline) != 1 || first.Timeline[0].Text != "先看项目。" || first.Timeline[0].At != got.Timeline[0].At {
		t.Fatalf("later chunks mutated an earlier snapshot: %+v", first)
	}
}

func TestToolUpdateNeverCreatesATimelineSpan(t *testing.T) {
	c := &collector{}
	c.handle(acp.SessionUpdate{SessionUpdate: acp.SessionUpdateTypeToolCallUpdate, ToolCallID: "late"})
	c.writeText("叙述")
	if got, _ := c.snapshot(); len(got.Timeline) != 1 {
		t.Fatalf("update created a tool span: %+v", got.Timeline)
	}
	c.handle(acp.SessionUpdate{SessionUpdate: acp.SessionUpdateTypeToolCall, ToolCallID: "late"})
	if got, _ := c.snapshot(); len(got.Timeline) != 2 || got.Timeline[1].Tool != "late" || len(got.Tools) != 1 {
		t.Fatalf("creation lost its position or duplicated details: %+v", got)
	}
}

func TestTimelineSharesTextAndThoughtRetentionLimits(t *testing.T) {
	c := &collector{}
	c.writeThought(strings.Repeat("前", 12000))
	c.writeText("中途叙述")
	c.writeThought(strings.Repeat("中", 20000))
	c.handle(acp.SessionUpdate{SessionUpdate: acp.SessionUpdateTypeToolCall, ToolCallID: "check"})
	c.writeThought(strings.Repeat("尾", 12000))
	c.writeThought("结尾")
	c.writeText(strings.Repeat("文", maxCollectBytes))
	c.writeText("超出上限")
	got, _ := c.snapshot()
	var thought, text string
	for _, span := range got.Timeline {
		if !utf8.ValidString(span.Text) {
			t.Fatal("timeline split a UTF-8 rune")
		}
		switch span.Kind {
		case "thought":
			thought += span.Text
		case "text":
			text += span.Text
		}
	}
	if thought != got.Reasoning || len(thought) > maxThoughtBytes || !strings.HasSuffix(thought, "结尾") || strings.Count(thought, "省略") != 1 {
		t.Fatal("timeline bypassed thought retention or lost its omission marker")
	}
	if text != got.Answer || len(text) > maxCollectBytes || !strings.HasSuffix(text, truncationMarker) {
		t.Fatal("timeline bypassed answer retention")
	}
}
