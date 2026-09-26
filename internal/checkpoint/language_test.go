package checkpoint

import (
	"strings"
	"testing"
	"unicode"

	"github.com/gopact-ai/steve/internal/i18n"
)

func containsHan(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return unicode.Is(unicode.Han, r) }) >= 0
}

// Every question a resume plan can ask is asked in the caller's language:
// its title, what was tried, what blocks it, why, what to do, and each
// option. Text the caller supplied is its own and passes through.
func TestResumeQuestionsAreAskedInTheCallersLanguage(t *testing.T) {
	en := i18n.New(i18n.LocaleEN)
	cases := map[string]func(*ResumeRequest){
		"source-incomplete":        func(r *ResumeRequest) { r.Source.TaskID = "" },
		"checkpoint-unavailable":   func(r *ResumeRequest) { r.Checkpoint = nil },
		"checkpoint-mismatch":      func(r *ResumeRequest) { r.Source.SessionID = "other-session" },
		"checkpoint-not-local":     func(r *ResumeRequest) { r.Target.NodeID = "node-c" },
		"writer-not-isolated":      func(r *ResumeRequest) { r.Isolation = IsolationEvidence{} },
		"actions-not-reconciled":   func(r *ResumeRequest) { r.Reconciliation.Checked = false },
		"execution-not-authorized": func(r *ResumeRequest) { r.Target.Authorized = false },
		"capabilities-unchecked":   func(r *ResumeRequest) { r.Target.AdmissionChecked = false },
		"capability-missing":       func(r *ResumeRequest) { r.Target.Missing = []CapabilityIssue{{Name: "project network"}} },
		"action-result-unknown": func(r *ResumeRequest) {
			r.Reconciliation.AdditionalUnknown = []ExternalAction{{ID: "action-9", Description: "create remote change request"}}
		},
		"action-result-conflict": func(r *ResumeRequest) {
			r.Reconciliation.Results = []ActionResolution{{ActionID: "a", Outcome: ActionApplied, Evidence: "x"}, {ActionID: "a", Outcome: ActionNotApplied, Evidence: "y"}}
		},
	}
	for code, breakIt := range cases {
		t.Run(code, func(t *testing.T) {
			req := safeResumeRequest(t)
			breakIt(&req)
			plan := ResumePlan(en, req)
			q := plan.Question
			if plan.Action != ResumeAskUser || q == nil || q.Code != code {
				t.Fatalf("plan = %+v", plan)
			}
			parts := append([]string{q.Title, q.Problem, q.Reason, q.Recommendation, q.Message(en)}, q.Attempted...)
			for _, o := range q.Options {
				parts = append(parts, o.Label)
			}
			for _, part := range parts {
				if part == "" || containsHan(part) {
					t.Fatalf("English question part %q in %+v", part, q)
				}
			}
			if !strings.HasPrefix(q.Message(en), "Tried: ") {
				t.Fatalf("message = %q", q.Message(en))
			}
		})
	}
}
