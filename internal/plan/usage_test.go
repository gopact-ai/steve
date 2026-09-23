package plan

import (
	"encoding/json"
	"testing"

	"github.com/gopact-ai/steve/internal/attempt"
)

// A step result's usage is the attempt record's usage: the same type, and
// the same encoding step results were stored with before they shared it.
func TestStepResultUsageKeepsItsStoredEncoding(t *testing.T) {
	const stored = `{"usage":{"model":"m","input":1,"output":2,"cached_read":3,"cached_write":4,"context":5,"reported":true}}`
	var result StepResult
	if err := json.Unmarshal([]byte(stored), &result); err != nil {
		t.Fatal(err)
	}
	want := attempt.Usage{Model: "m", Input: 1, Output: 2, CachedRead: 3, CachedWrite: 4, Context: 5, Reported: true}
	var got *attempt.Usage = result.Usage
	if got == nil || *got != want {
		t.Fatalf("decoded usage = %+v, want %+v", got, want)
	}
	raw, err := json.Marshal(StepResult{Usage: &want})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != stored {
		t.Fatalf("encoded %s, want %s", raw, stored)
	}
	if raw, _ := json.Marshal(StepResult{Usage: &attempt.Usage{}}); string(raw) != `{"usage":{"reported":false}}` {
		t.Fatalf("empty usage encoded %s", raw)
	}
}
