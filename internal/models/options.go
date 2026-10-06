package models

import "github.com/gopact-ai/steve/internal/view"

// SelectorsOf preserves the descriptor and confirmed current value from a
// session observation. Boolean options intentionally have no choice table.
func SelectorsOf(options []view.Option) []Selector {
	var out []Selector
	for _, option := range options {
		selector := Selector{ID: option.ID, Name: option.Name, Category: option.Category, Type: option.Type, Current: option.Current}
		if option.Type != "boolean" {
			for _, choice := range option.Choices {
				label := choice.Label
				if label == "" {
					label = choice.Value
				}
				selector.Choices = append(selector.Choices, label)
				selector.Values = append(selector.Values, choice.Value)
			}
		}
		out = append(out, selector)
	}
	return out
}
