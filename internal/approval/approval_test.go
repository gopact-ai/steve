package approval

import "testing"

// The two vocabularies Steve meets in practice, as the agents report them.
var (
	codex  = []Mode{{Value: "read-only", Label: "Ask for approval"}, {Value: "agent", Label: "Approve for me"}, {Value: "agent-full-access", Label: "Full access"}}
	claude = []Mode{{Value: "default", Label: "Manual"}, {Value: "acceptEdits", Label: "Accept edits"}, {Value: "plan", Label: "Plan"}, {Value: "auto", Label: "Auto"}, {Value: "bypassPermissions", Label: "Bypass permissions"}}
)

func TestResolveOneIntentAcrossVocabularies(t *testing.T) {
	for _, row := range []struct{ intent, codex, claude string }{
		{Ask, "read-only", "default"},
		{Auto, "agent", "acceptEdits"},
		{Full, "agent-full-access", "bypassPermissions"},
	} {
		if got, ok := Resolve(row.intent, codex); !ok || got.Value != row.codex {
			t.Fatalf("codex %s = %q (%v), want %q", row.intent, got.Value, ok, row.codex)
		}
		if got, ok := Resolve(row.intent, claude); !ok || got.Value != row.claude {
			t.Fatalf("claude %s = %q (%v), want %q", row.intent, got.Value, ok, row.claude)
		}
	}
}

// A planning mode is not an approval level, and no stance may select it.
func TestPlanningModeIsNeverAnApprovalLevel(t *testing.T) {
	for _, mode := range []Mode{{Value: "plan", Label: "Plan"}, {Value: "plan", Label: "Planning"}} {
		if rank := Rank(mode); rank != "" {
			t.Fatalf("mode %q ranked %q", mode.Value, rank)
		}
	}
	for _, intent := range []string{Ask, Auto, Full} {
		if got, ok := Resolve(intent, []Mode{{Value: "plan", Label: "Plan"}}); ok {
			t.Fatalf("%s resolved to %q on a tool that only plans", intent, got.Value)
		}
	}
}

// An unset default changes nothing, and a tool whose modes say nothing
// recognisable is left in the mode its owner's tool chose.
func TestNoDefaultAndNoMatchLeaveTheAgentAlone(t *testing.T) {
	if _, ok := Resolve("", codex); ok {
		t.Fatal("an empty intent resolved to a mode")
	}
	if got, ok := Resolve(Full, []Mode{{Value: "mode-1", Label: "Mode 1"}, {Value: "", Label: "Full access"}}); ok {
		t.Fatalf("unknown vocabulary resolved to %q", got.Value)
	}
	if !Valid("") || !Valid(Auto) || Valid("automode") {
		t.Fatal("accepted settings values are wrong")
	}
}
