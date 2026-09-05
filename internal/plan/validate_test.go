package plan

import (
	"strings"
	"testing"
)

func TestOverlappingTouchesOnlyMatterForStepsThatCanRunAtOnce(t *testing.T) {
	step := func(id string, touches []string, needs ...string) Step {
		return Step{ID: id, Goal: id, Agent: "a", Needs: needs, Touches: touches, Verify: &Verify{Kind: VerifyNone, Why: "t"}}
	}
	parallel := Plan{Steps: []Step{step("a", []string{"pkg/"}), step("b", []string{"pkg/x.go"})}}
	if err := Validate(parallel); err == nil || !strings.Contains(err.Error(), "pkg/") {
		t.Fatalf("overlapping parallel touches accepted: %v", err)
	}
	disjoint := Plan{Steps: []Step{step("a", []string{"pkg/"}), step("b", []string{"docs/"})}}
	if err := Validate(disjoint); err != nil {
		t.Fatal(err)
	}
	ordered := Plan{Steps: []Step{step("a", []string{"pkg/"}), step("b", []string{"pkg/"}, "a")}}
	if err := Validate(ordered); err != nil {
		t.Fatalf("ordered steps sharing paths were refused: %v", err)
	}
	if !Covers([]string{"pkg/"}, "pkg/sub/x.go") || Covers([]string{"pkg"}, "pkg/x.go") || !Covers([]string{"./a.go"}, "a.go") {
		t.Fatal("Covers has the wrong idea of a prefix")
	}
}
