package sshconnect

import (
	"errors"
	"strings"
	"testing"
	"unicode"

	"github.com/gopact-ai/steve/internal/i18n"
)

func hasHan(text string) bool { return strings.IndexFunc(text, isHan) >= 0 }

func isHan(r rune) bool { return unicode.Is(unicode.Han, r) }

// An installation speaks the language of whoever started it: the checks
// it reports, the lines it writes into its log and the failure it ends
// with, whose advice follows the English way of joining a sentence. The
// log is written once; reading it later does not translate it.
func TestAnInstallationSpeaksTheLanguageOfWhoeverStartedIt(t *testing.T) {
	svc, r, _, _ := serviceFixture(t)
	svc.runner = &talkativeRunner{recordingRunner: r, installStderr: "remote refused"}
	english := i18n.WithLocale(t.Context(), i18n.LocaleEN)
	plan, err := svc.Plan(english, installRequest())
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range plan.Check.Steps {
		if hasHan(step.Message + step.Suggestion) {
			t.Errorf("check step %s is in Chinese: %q %q", step.ID, step.Message, step.Suggestion)
		}
	}
	result, err := svc.Commit(english, plan.ID)
	var step *StepError
	if !errors.As(err, &step) {
		t.Fatalf("commit error = %v, want a failed step", err)
	}
	if hasHan(err.Error()) || err.Error() != step.Message+"; "+step.Suggestion {
		t.Errorf("failure = %q, want English with its advice after \"; \"", err.Error())
	}
	for _, line := range result.Log {
		if line.Stream == "steve" && hasHan(line.Text) {
			t.Errorf("log line is in Chinese: %q", line.Text)
		}
	}
	status, err := svc.Status(plan.ID)
	if err != nil || logText(status) != logText(result) {
		t.Fatalf("the log read back = %q %v, want it as written: %q", logText(status), err, logText(result))
	}
}

// A refusal before anything is opened is said in the asker's language.
func TestARefusedRequestIsSaidInTheAskersLanguage(t *testing.T) {
	svc, _, _, _ := serviceFixture(t)
	request := installRequest()
	request.Addr = "nowhere"
	_, err := svc.Plan(i18n.WithLocale(t.Context(), i18n.LocaleEN), request)
	if err == nil || hasHan(err.Error()) {
		t.Fatalf("refusal = %v, want it in English", err)
	}
	_, err = svc.Plan(i18n.WithLocale(t.Context(), i18n.LocaleZH), request)
	if err == nil || !hasHan(err.Error()) {
		t.Fatalf("refusal = %v, want it in Chinese", err)
	}
}
