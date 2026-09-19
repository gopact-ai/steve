package readmodel

import (
	"encoding/json"
	"testing"

	"github.com/gopact-ai/steve/internal/task"
)

func TestTaskProjectionPreservesExplicitTransport(t *testing.T) {
	for _, tc := range []struct{ transport, conversation string }{
		{"feishu", "console:opaque-native-id"},
		{"console", "opaque-without-prefix"},
		{"", "console:unknown"},
	} {
		t.Run(tc.transport, func(t *testing.T) {
			projected := tasks([]task.Task{{ID: "fixture", Transport: tc.transport, Channel: tc.conversation}}, nil)
			raw, err := json.Marshal(projected[0])
			if err != nil {
				t.Fatal(err)
			}
			var wire map[string]any
			if err := json.Unmarshal(raw, &wire); err != nil {
				t.Fatal(err)
			}
			if got, _ := wire["transport"].(string); got != tc.transport {
				t.Fatalf("transport must be projected explicitly, never inferred from channel: %s", raw)
			}
			if wire["channel"] != tc.conversation {
				t.Fatalf("opaque conversation changed: %s", raw)
			}
		})
	}
}
