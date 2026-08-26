package turn

import (
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/view"
)

func choices() []view.Choice {
	return []view.Choice{
		{Value: "gpt-5.6-sol", Label: "GPT 5.6 Sol"},
		{Value: "gpt-5.6-terra", Label: "GPT 5.6 Terra"},
		{Value: "opus[1m]", Label: "Opus 5 (1M context)"},
	}
}

func TestMatchModel(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
		err               error
	}{
		{"exact id", "gpt-5.6-sol", "gpt-5.6-sol", nil},
		{"exact label", "GPT 5.6 Sol", "gpt-5.6-sol", nil},
		{"label wrong case", "opus 5 (1m context)", "opus[1m]", nil},
		{"unique substring", "terra", "gpt-5.6-terra", nil},
		{"substring of a label", "1M", "opus[1m]", nil},
		{"nothing matches", "haiku", "", errModelUnknown},
		{"matches several", "gpt", "", errModelAmbiguous},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := matchModel(choices(), tc.input)
			if err != tc.err {
				t.Fatalf("err = %v, want %v", err, tc.err)
			}
			if err == nil && got.Value != tc.want {
				t.Fatalf("model = %q, want %q", got.Value, tc.want)
			}
		})
	}
}

// An exact id must win even when it is also a substring of other options, so
// a real model name is never rejected as ambiguous.
func TestMatchModelPrefersExactOverAmbiguous(t *testing.T) {
	list := []view.Choice{
		{Value: "sonnet", Label: "Sonnet"},
		{Value: "sonnet[1m]", Label: "Sonnet 5 (1M context)"},
	}
	got, err := matchModel(list, "sonnet")
	if err != nil {
		t.Fatalf("err = %v, want the exact id to win", err)
	}
	if got.Value != "sonnet" {
		t.Fatalf("model = %q", got.Value)
	}
}

func TestModelListMarksCurrentAndCaps(t *testing.T) {
	got := modelList(choices(), "GPT 5.6 Terra")
	if !strings.Contains(got, "▸ GPT 5.6 Terra") {
		t.Fatalf("current model not marked:\n%s", got)
	}
	if strings.Count(got, "▸") != 1 {
		t.Fatalf("more than one model marked current:\n%s", got)
	}
	many := make([]view.Choice, 0, 30)
	for i := 0; i < 30; i++ {
		many = append(many, view.Choice{Value: string(rune('a' + i)), Label: string(rune('a' + i))})
	}
	got = modelList(many, "")
	if !strings.Contains(got, "… +18") {
		t.Fatalf("long list not capped:\n%s", got)
	}
}
