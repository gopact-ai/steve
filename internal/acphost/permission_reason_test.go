package acphost

import (
	"strings"
	"testing"

	"github.com/gopact-ai/acp"
)

// A permission request must say what is about to happen, not just that a
// tool wants to run: the command, where it runs, which files it touches.
func TestPermissionReasonDescribesTheToolCall(t *testing.T) {
	kind := acp.ToolKindExecute
	command := permissionReason(acp.ToolCallUpdate{Kind: &kind, RawInput: map[string]any{"command": []any{"system_profiler", "SPHardwareDataType"}, "cwd": "/work/p"}})
	for _, want := range []string{"```", "system_profiler SPHardwareDataType", "`/work/p`"} {
		if !strings.Contains(command, want) {
			t.Fatalf("command reason lacks %q:\n%s", want, command)
		}
	}
	described := permissionReason(acp.ToolCallUpdate{RawInput: map[string]any{"command": "rm -rf build", "description": "Clean the build tree"}})
	if !strings.Contains(described, "rm -rf build") || !strings.Contains(described, "Clean the build tree") {
		t.Fatalf("description missing:\n%s", described)
	}
	edit := permissionReason(acp.ToolCallUpdate{RawInput: map[string]any{"file_path": "/work/p/main.go", "old_string": strings.Repeat("x", 5000), "new_string": "y"}, Content: &[]acp.ToolCallContent{{Type: acp.ToolCallContentTypeDiff, Path: "/work/p/main.go", NewText: "y"}}})
	if !strings.Contains(edit, "`/work/p/main.go`") || strings.Contains(edit, "xxxx") {
		t.Fatalf("edit reason should name the file without dumping its contents:\n%s", edit)
	}
	unknown := permissionReason(acp.ToolCallUpdate{RawInput: map[string]any{"url": "https://example.com", "depth": 2}})
	if !strings.Contains(unknown, "https://example.com") || !strings.Contains(unknown, "depth") {
		t.Fatalf("unknown input should still be shown:\n%s", unknown)
	}
	long := permissionReason(acp.ToolCallUpdate{RawInput: map[string]any{"command": strings.Repeat("a", 10000)}})
	if len(long) > 5000 || !strings.Contains(long, "…") {
		t.Fatalf("reason should be bounded, got %d bytes", len(long))
	}
	locations := []acp.ToolCallLocation{{Path: "/work/p/a.go"}, {Path: "/work/p/b.go"}}
	if got := permissionReason(acp.ToolCallUpdate{Locations: &locations}); !strings.Contains(got, "`/work/p/a.go`") || !strings.Contains(got, "`/work/p/b.go`") {
		t.Fatalf("locations missing:\n%s", got)
	}
	if got := permissionReason(acp.ToolCallUpdate{}); got != "" {
		t.Fatalf("empty call should give an empty reason, got %q", got)
	}
}
