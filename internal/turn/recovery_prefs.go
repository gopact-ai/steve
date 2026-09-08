package turn

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/harness"
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
func applyRecoveryPreferences(ctx context.Context, runner harness.Runner, preferences *attempt.SessionPreferences) error {
	if preferences == nil || preferences.Model == "" && len(preferences.Options) == 0 {
		return nil
	}
	configurable, ok := runner.(harness.Configurable)
	if !ok {
		return errors.New("目标Agent不能确认原执行的模型和选项")
	}
	if preferences.Model != "" {
		id, choices := configurable.ModelChoices()
		picked, ok := harness.MatchChoice(choices, preferences.Model)
		if !ok || id == "" {
			if configurable.Settings().Model != preferences.Model {
				return fmt.Errorf("目标Agent不支持原执行模型 %s", preferences.Model)
			}
		} else {
			if current := configurable.Settings().Model; current != picked.Value && current != picked.Label {
				if err := configurable.SetModel(ctx, id, picked.Value); err != nil {
					return fmt.Errorf("设置原执行模型 %s: %w", preferences.Model, err)
				}
			}
			actual := configurable.Settings().Model
			if actual != picked.Value && actual != picked.Label {
				return fmt.Errorf("目标Agent未确认使用模型 %s", preferences.Model)
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
			return fmt.Errorf("目标Agent缺少原执行选项 %s", id)
		}
		picked, ok := harness.MatchChoice(option.Choices, want)
		if !ok {
			return fmt.Errorf("目标Agent不支持原执行选项 %s=%s", id, want)
		}
		if option.Current != picked.Value {
			if err := configurable.SetOption(ctx, id, picked.Value); err != nil {
				return fmt.Errorf("设置原执行选项 %s: %w", id, err)
			}
		}
		confirmed := false
		for _, actual := range configurable.Settings().Options {
			if actual.ID == id && actual.Current == picked.Value {
				confirmed = true
			}
		}
		if !confirmed {
			return fmt.Errorf("目标Agent未确认选项 %s=%s", id, want)
		}
	}
	return nil
}
