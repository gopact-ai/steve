package harness

import (
	"context"
	"testing"

	"github.com/gopact-ai/steve/internal/view"
)

// fakeConfigurable exposes a model selector and a reasoning-effort
// selector and records what was set.
type fakeConfigurable struct {
	Runner
	set map[string]string
}

func (f *fakeConfigurable) Settings() view.Settings {
	return view.Settings{Options: []view.Option{
		{ID: "model", Category: "model", Current: "gpt-5", Choices: []view.Choice{{Value: "gpt-5", Label: "GPT 5"}, {Value: "gpt-5-mini", Label: "GPT 5 mini"}}},
		{ID: "reasoning_effort", Name: "Reasoning effort", Current: "medium", Choices: []view.Choice{{Value: "low", Label: "Low"}, {Value: "medium", Label: "Medium"}, {Value: "high", Label: "High"}}},
	}}
}
func (f *fakeConfigurable) ModelChoices() (string, []view.Choice) {
	return "model", []view.Choice{{Value: "gpt-5", Label: "GPT 5"}, {Value: "gpt-5-mini", Label: "GPT 5 mini"}}
}
func (f *fakeConfigurable) SetModel(_ context.Context, id, value string) error {
	f.set[id] = value
	return nil
}
func (f *fakeConfigurable) SetOption(_ context.Context, id, value string) error {
	f.set[id] = value
	return nil
}

// An agent's pins reach the session by option id: the model by name, any
// other selector by its label or value; a value already in force is not
// set again; an unknown selector or choice is skipped, never fatal.
func TestApplyPreferencesPinsEverySelector(t *testing.T) {
	f := &fakeConfigurable{set: map[string]string{}}
	ApplyPreferences(context.Background(), f, "a", "GPT 5 mini", map[string]string{"reasoning_effort": "High", "thinking": "deep", "model": "ignored"})
	if f.set["model"] != "gpt-5-mini" || f.set["reasoning_effort"] != "high" {
		t.Fatalf("set = %v", f.set)
	}
	if _, touched := f.set["thinking"]; touched {
		t.Fatal("an unknown selector was set")
	}
	f = &fakeConfigurable{set: map[string]string{}}
	ApplyPreferences(context.Background(), f, "a", "", map[string]string{"reasoning_effort": "medium"})
	if len(f.set) != 0 {
		t.Fatalf("a value already in force was set again: %v", f.set)
	}
	if c, ok := MatchChoice([]view.Choice{{Value: "hi", Label: "High"}, {Value: "hu", Label: "Huge"}}, "h"); ok {
		t.Fatalf("an ambiguous prefix matched %v", c)
	}
	if c, ok := MatchChoice([]view.Choice{{Value: "hi", Label: "High"}, {Value: "lo", Label: "Low"}}, "hig"); !ok || c.Value != "hi" {
		t.Fatalf("a unique prefix did not match: %v %v", c, ok)
	}
}
