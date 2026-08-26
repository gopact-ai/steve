package setup

import (
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/i18n"
)

func TestMatchChoice(t *testing.T) {
	options := []option{
		{methodCreate, "create app"},
		{methodManual, "paste credentials"},
	}
	tests := []struct {
		name  string
		input string
		want  string
		ok    bool
	}{
		{name: "empty uses default", input: "", want: methodCreate, ok: true},
		{name: "digit", input: "2", want: methodManual, ok: true},
		{name: "digit with paren", input: "2)", want: methodManual, ok: true},
		{name: "fullwidth digit", input: "２", want: methodManual, ok: true},
		{name: "value name", input: "create", want: methodCreate, ok: true},
		{name: "arrow leftover then digit", input: "\x1b[A1", want: methodCreate, ok: true},
		{name: "unknown", input: "x", want: "", ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := matchChoice(tt.input, options, methodCreate)
			if ok != tt.ok || got != tt.want {
				t.Fatalf("matchChoice(%q) = %q, %v; want %q, %v", tt.input, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestDrawSelectMenuLineCount(t *testing.T) {
	var out strings.Builder
	options := []option{{"a", "one"}, {"b", "two"}, {"c", "three"}}
	if got := drawSelectMenu(&out, options, 1, i18n.New(i18n.LocaleZH)); got != 4 {
		t.Fatalf("drawSelectMenu lines = %d, want 4", got)
	}
	if strings.Count(out.String(), "\n") != 4 {
		t.Fatalf("printed newlines = %d, want 4\n%s", strings.Count(out.String(), "\n"), out.String())
	}
}

func TestReadSelectKey(t *testing.T) {
	up, err := readSelectKey(strings.NewReader("\x1b[A"))
	if err != nil || up.kind != selectKeyUp {
		t.Fatalf("up = %#v, %v", up, err)
	}
	two, err := readSelectKey(strings.NewReader("2"))
	if err != nil || two.kind != selectKeyDigit || two.digit != 2 {
		t.Fatalf("two = %#v, %v", two, err)
	}
	enter, err := readSelectKey(strings.NewReader("\r"))
	if err != nil || enter.kind != selectKeyEnter {
		t.Fatalf("enter = %#v, %v", enter, err)
	}
}
