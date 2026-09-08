package turn

import (
	"context"
	"errors"
	"testing"

	"github.com/gopact-ai/steve/internal/attempt"
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
			err := applyRecoveryPreferences(t.Context(), r, preferences)
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
