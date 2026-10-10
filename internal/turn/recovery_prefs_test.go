package turn

import (
	"context"
	"errors"
	"testing"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/view"
)

type recoveryConfigurable struct {
	*fakeRunner
	settings view.Settings
	refused  bool
	ignore   bool
}

func (r *recoveryConfigurable) Settings() view.Settings { return r.settings }
func (r *recoveryConfigurable) ModelChoices() (string, []view.Choice) {
	return "model", []view.Choice{{Value: "m1", Label: "Model 1"}, {Value: "m2", Label: "Model 2"}}
}
func (r *recoveryConfigurable) SetModel(ctx context.Context, id, value string) error {
	if r.refused {
		return errors.New("model unavailable")
	}
	if !r.ignore {
		r.settings.Model = value
	}
	return nil
}
func (r *recoveryConfigurable) SetOption(ctx context.Context, id, value string) error {
	if r.refused {
		return errors.New("option unavailable")
	}
	if !r.ignore {
		for i := range r.settings.Options {
			if r.settings.Options[i].ID == id {
				r.settings.Options[i].Current = value
			}
		}
	}
	return nil
}

func TestRecoveryPreferencesMustBeConfirmedBeforeTaskInput(t *testing.T) {
	preferences := &attempt.SessionPreferences{Model: "m2", Options: map[string]string{"reasoning": "high"}}
	for _, mode := range []string{"supported", "refused", "ignored", "missing"} {
		t.Run(mode, func(t *testing.T) {
			r := &recoveryConfigurable{fakeRunner: &fakeRunner{}, settings: view.Settings{Model: "m1", Options: []view.Option{{ID: "reasoning", Current: "low", Choices: []view.Choice{{Value: "low"}, {Value: "high"}}}}}, refused: mode == "refused", ignore: mode == "ignored"}
			if mode == "missing" {
				r.settings.Options = nil
			}
			err := applyRecoveryPreferences(t.Context(), i18n.New(i18n.LocaleZH), r, preferences)
			if mode == "supported" {
				if err != nil {
					t.Fatal(err)
				}
				actual := sessionPreferences(r)
				if actual.Model != "m2" || actual.Options["reasoning"] != "high" {
					t.Fatalf("effective settings lost: %+v", actual)
				}
			} else if err == nil {
				t.Fatal("recovery silently fell back to other model/settings")
			}
			if len(r.seen()) != 0 {
				t.Fatal("preference check sent task input")
			}
		})
	}
}

// The record of a turn says what the turn ran with. After the owner
// changes approval mode, the agent's configured default is the wrong
// answer: the session's own report is the right one, by its label.
func TestTheTurnRecordReportsTheSessionsOwnSelectors(t *testing.T) {
	configured := agent.Agent{ID: "dev", Model: "m1", Options: map[string]string{"mode": "Full access"}}
	live := &recoveryConfigurable{fakeRunner: &fakeRunner{}, settings: view.Settings{Model: "Model 2", Options: []view.Option{
		{ID: "mode", Category: "mode", Current: "read-only", Choices: []view.Choice{{Value: "read-only", Label: "Ask for approval"}, {Value: "agent-full-access", Label: "Full access"}}},
	}}}
	model, options := sessionSelectors(live, configured)
	if model != "Model 2" || options["mode"] != "Ask for approval" {
		t.Fatalf("turn record kept the agent's default: %q %v", model, options)
	}
	plain, fallback := sessionSelectors(&fakeRunner{}, configured)
	if plain != "m1" || fallback["mode"] != "Full access" {
		t.Fatalf("an agent without selectors lost its configuration: %q %v", plain, fallback)
	}
}

func TestBooleanRecoveryPreferencesKeepFalseDistinctFromUnset(t *testing.T) {
	for _, tc := range []struct {
		name, actual, want       string
		ignore, refused, success bool
	}{
		{"false", "true", "false", false, false, true},
		{"true", "false", "true", false, false, true},
		{"confirmed false", "false", "false", false, false, true},
		{"unreported is not false", "", "false", true, false, false},
		{"corrected actual", "true", "false", true, false, false},
		{"invalid bool", "false", "0", false, false, false},
		{"RPC refused", "true", "false", false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &recoveryConfigurable{fakeRunner: &fakeRunner{}, settings: view.Settings{Options: []view.Option{{ID: "vendor-toggle", Type: "boolean", Category: "vendor/private", Current: tc.actual}}}, ignore: tc.ignore, refused: tc.refused}
			prefs := &attempt.SessionPreferences{Options: map[string]string{"vendor-toggle": tc.want}}
			err := applyRecoveryPreferences(t.Context(), i18n.New(i18n.LocaleZH), r, prefs)
			if (err == nil) != tc.success {
				t.Fatalf("recovery = %v; success=%v", err, tc.success)
			}
			if tc.success && sessionPreferences(r).Options["vendor-toggle"] != tc.want {
				t.Fatal("frozen false was omitted")
			}
			if len(r.seen()) != 0 {
				t.Fatal("preference recovery sent a business prompt")
			}
		})
	}
}

func TestLivePreferenceResultMeansConfirmedActualNotAcceptedRequest(t *testing.T) {
	c, rt, _ := selectorCoordinator(t, true)
	r := rt.runner.(*recoveryConfigurable)
	r.settings.Options = []view.Option{{ID: "vendor-toggle", Type: "boolean", Current: "true"}}
	c.beginTurn("chat", "grok", func() {})
	defer c.clearActive("chat", "grok")
	c.setRunner("chat", "grok", r)
	r.ignore = true
	if c.applyLive(t.Context(), "chat", "grok", map[string]string{"vendor-toggle": "false"}) {
		t.Fatal("ignored false was reported as applied")
	}
	r.ignore = false
	if !c.applyLive(t.Context(), "chat", "grok", map[string]string{"vendor-toggle": "false"}) {
		t.Fatal("confirmed false was not applied")
	}
	if sessionPreferences(r).Options["vendor-toggle"] != "false" {
		t.Fatal("confirmed false was omitted")
	}
}
