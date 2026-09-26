package turn

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/view"
)

func sessionPreferences(runner harness.Runner) *attempt.SessionPreferences {
	configurable, ok := runner.(harness.Configurable)
	if !ok {
		return nil
	}
	settings := configurable.Settings()
	p := &attempt.SessionPreferences{Model: settings.Model, ModelLabel: settings.Model, Options: map[string]string{}}
	_, choices := configurable.ModelChoices()
	if picked, ok := harness.MatchChoice(choices, p.Model); ok {
		p.Model = picked.Value
	}
	for _, option := range settings.Options {
		if option.Category == "model" {
			if option.Current != "" {
				p.Model = option.Current
			}
			continue
		}
		if option.Current != "" {
			p.Options[option.ID] = option.Current
		}
	}
	return p
}

// Recovery requires every frozen selector to be supported and confirmed by
// the new native session before a user prompt can be sent.
func applyRecoveryPreferences(ctx context.Context, text i18n.Catalog, runner harness.Runner, preferences *attempt.SessionPreferences) error {
	if preferences == nil || preferences.Model == "" && len(preferences.Options) == 0 {
		return nil
	}
	configurable, ok := runner.(harness.Configurable)
	if !ok {
		return errors.New(text.T(i18n.PrefsUnconfirmable))
	}
	if preferences.Model != "" {
		id, choices := configurable.ModelChoices()
		picked, ok := harness.MatchChoice(choices, preferences.Model)
		if !ok || id == "" {
			if configurable.Settings().Model != preferences.Model {
				return errors.New(text.T(i18n.PrefsModelUnsupported, preferences.Model))
			}
		} else {
			if current := configurable.Settings().Model; current != picked.Value && current != picked.Label {
				if err := configurable.SetModel(ctx, id, picked.Value); err != nil {
					return fmt.Errorf("%s: %w", text.T(i18n.PrefsModelSet, preferences.Model), err)
				}
			}
			actual := configurable.Settings().Model
			if actual != picked.Value && actual != picked.Label {
				return errors.New(text.T(i18n.PrefsModelUnconfirmed, preferences.Model))
			}
		}
	}
	ids := make([]string, 0, len(preferences.Options))
	for id := range preferences.Options {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		want := preferences.Options[id]
		var option *view.Option
		for _, exposed := range configurable.Settings().Options {
			if exposed.ID == id {
				option = &exposed
				break
			}
		}
		if option == nil {
			return errors.New(text.T(i18n.PrefsOptionMissing, id))
		}
		picked, ok := harness.MatchChoice(option.Choices, want)
		if !ok {
			return errors.New(text.T(i18n.PrefsOptionUnsupported, id, want))
		}
		if option.Current != picked.Value {
			if err := configurable.SetOption(ctx, id, picked.Value); err != nil {
				return fmt.Errorf("%s: %w", text.T(i18n.PrefsOptionSet, id), err)
			}
		}
		confirmed := false
		for _, actual := range configurable.Settings().Options {
			if actual.ID == id && actual.Current == picked.Value {
				confirmed = true
			}
		}
		if !confirmed {
			return errors.New(text.T(i18n.PrefsOptionUnconfirmed, id, want))
		}
	}
	return nil
}

// sessionSelectors is what the session actually holds when the turn starts:
// the conversation's choices as the agent reports them, by the label it
// gave them, falling back to the agent's configuration when the harness
// exposes no selectors. The record of a turn has to say what it ran with,
// not what the agent would have run with by default.
func sessionSelectors(runner harness.Runner, selected agent.Agent) (string, map[string]string) {
	configurable, ok := runner.(harness.Configurable)
	if !ok {
		return selected.Model, selected.Options
	}
	settings := configurable.Settings()
	model := settings.Model
	if model == "" {
		model = selected.Model
	}
	options := map[string]string{}
	for _, option := range settings.Options {
		if option.Category == "model" || option.Current == "" {
			continue
		}
		options[option.ID] = option.Current
		if choice, ok := harness.MatchChoice(option.Choices, option.Current); ok && choice.Label != "" {
			options[option.ID] = choice.Label
		}
	}
	if len(options) == 0 {
		return model, selected.Options
	}
	return model, options
}
