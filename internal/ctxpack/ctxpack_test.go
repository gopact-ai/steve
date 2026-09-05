package ctxpack

import (
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/plan"
)

// A fresh directory is a normal place to start. The bearings text once told
// agents to confirm the git state first and to stop if "the premise did not
// hold"; a real agent read an empty, non-git directory as exactly that and
// stopped without doing the work. The text must make the common case
// explicitly fine.
func TestBearingsDoNotStopOnAnEmptyDirectory(t *testing.T) {
	text := Bearings("/w", nil)
	for _, want := range []string{"空目录", "正常的起点", "不要因此停下", "没有就跳过"} {
		if !strings.Contains(text, want) {
			t.Errorf("bearings should say %q", want)
		}
	}
	if strings.Contains(text, "前提不成立") {
		t.Error("bearings still tell the agent to stop when the premise does not hold")
	}
	withRef := Bearings("/w", []plan.Ref{{Kind: "git", Value: "abc123"}})
	if !strings.Contains(withRef, "abc123") {
		t.Error("a prior git ref should be pointed at")
	}
}

func TestBuildRefusesOversizedContext(t *testing.T) {
	big := Context{Goal: strings.Repeat("x", MaxBytes+1)}
	if _, err := Build(big); err == nil {
		t.Fatal("an oversized context was accepted instead of being sent back as a ref")
	}
	small := Context{Goal: "fine", Facts: []string{"gpu"}, TurnsLeft: 3}
	if _, err := Build(small); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(small.Render(), "只推进这一步") {
		t.Error("the budget line should tell the agent to do this step only")
	}
}
