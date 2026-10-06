package harness

import (
	"context"
	"reflect"
	"testing"

	"github.com/gopact-ai/steve/internal/view"
)

type booleanPreferencesRunner struct {
	*fakeConfigurable
	options      []view.Option
	observations []view.Settings
}

func (r *booleanPreferencesRunner) Settings() view.Settings {
	return view.Settings{Options: r.options}
}

func (r *booleanPreferencesRunner) Reobserve() {
	r.observations = append(r.observations, r.Settings())
}

func TestBooleanPreferencesKeepFalseDistinctFromUnspecified(t *testing.T) {
	for _, tc := range []struct {
		name, current, want string
		pinned              bool
		set                 map[string]string
	}{
		{name: "false", current: "true", want: "false", pinned: true, set: map[string]string{"toggle": "false"}},
		{name: "true", current: "false", want: "true", pinned: true, set: map[string]string{"toggle": "true"}},
		{name: "unset", current: "true", set: map[string]string{}},
		{name: "current-false", current: "false", want: "false", pinned: true, set: map[string]string{}},
		{name: "current-unset", want: "false", pinned: true, set: map[string]string{"toggle": "false"}},
		{name: "empty", want: "", pinned: true, set: map[string]string{}},
		{name: "bad-case", want: "False", pinned: true, set: map[string]string{}},
		{name: "bad-numeric", want: "0", pinned: true, set: map[string]string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &booleanPreferencesRunner{
				fakeConfigurable: &fakeConfigurable{set: map[string]string{}},
				options:          []view.Option{{ID: "toggle", Type: "boolean", Category: "opaque/private", Current: tc.current}},
			}
			var pins map[string]string
			if tc.pinned {
				pins = map[string]string{"toggle": tc.want}
			}
			ApplyPreferences(t.Context(), runner, "fixture", "", pins, "")
			if !reflect.DeepEqual(runner.set, tc.set) {
				t.Fatalf("requested options=%v, want %v", runner.set, tc.set)
			}
			if len(tc.set) > 0 {
				if len(runner.observations) != 1 || runner.observations[0].Options[0].Current != tc.current {
					t.Fatalf("reobservation invented the requested Actual: %+v", runner.observations)
				}
			}
		})
	}
}

func TestBooleanPreferencesDoNotTurnModeOrModelIntoToggles(t *testing.T) {
	for _, option := range []view.Option{
		{ID: "reserved", Type: "boolean", Category: "model"},
		{ID: "reserved", Type: "boolean", Category: "mode"},
		{ID: "mode", Type: "boolean", Category: "opaque"},
		{ID: "model", Type: "boolean", Category: "opaque"},
	} {
		runner := &booleanPreferencesRunner{
			fakeConfigurable: &fakeConfigurable{set: map[string]string{}},
			options:          []view.Option{option},
		}
		ApplyPreferences(context.Background(), runner, "fixture", "", map[string]string{option.ID: "false"}, "full")
		if len(runner.set) > 0 {
			t.Fatalf("reserved selector became boolean: %v", runner.set)
		}
	}
}

func TestBooleanPreferencesLeaveSelectFalseAnOpaqueID(t *testing.T) {
	runner := &booleanPreferencesRunner{
		fakeConfigurable: &fakeConfigurable{set: map[string]string{}},
		options: []view.Option{{
			ID: "opaque", Type: "select", Current: "old",
			Choices: []view.Choice{{Value: "false", Label: "Literal false"}},
		}},
	}
	ApplyPreferences(t.Context(), runner, "fixture", "", map[string]string{"opaque": "false"}, "")
	if runner.set["opaque"] != "false" {
		t.Fatal("select false was not matched by value")
	}
}
