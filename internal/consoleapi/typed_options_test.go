package consoleapi_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/models"
	"github.com/gopact-ai/steve/internal/readmodel"
	"github.com/gopact-ai/steve/internal/view"
)

func TestTypedOptionDTOsKeepFalseDistinctFromMissing(t *testing.T) {
	options := []view.Option{
		{ID: "toggle", Name: "Toggle", Type: "boolean", Category: "opaque/category", Current: "false"},
		{ID: "unset", Name: "Unset", Type: "boolean"},
		{ID: "literal", Name: "Literal", Type: "select", Current: "false", Choices: []view.Choice{{Value: "false", Label: "Opaque false"}}},
	}
	selectors := consoleapi.Selectors{Options: options, Preferred: map[string]string{"toggle": "true"}}
	raw, err := json.Marshal(selectors)
	if err != nil {
		t.Fatal(err)
	}
	var restored consoleapi.Selectors
	if err := json.Unmarshal(raw, &restored); err != nil || !reflect.DeepEqual(selectors, restored) {
		t.Fatalf("session DTO lost typed state: %s %v", raw, err)
	}
	observed := models.SelectorsOf(options)
	desktop := []consoleapi.DesktopAgentSelector{}
	harness := []readmodel.HarnessSelector{}
	for _, option := range observed {
		desktop = append(desktop, consoleapi.DesktopAgentSelector{ID: option.ID, Name: option.Name, Type: option.Type, Category: option.Category, Current: option.Current, Choices: option.Choices, Values: option.Values})
		harness = append(harness, readmodel.HarnessSelector{ID: option.ID, Name: option.Name, Type: option.Type, Category: option.Category, Current: option.Current, Choices: option.Choices, Values: option.Values})
	}
	for _, dto := range []any{desktop, harness, readmodel.Agent{Selectors: observed}} {
		raw, err := json.Marshal(dto)
		if err != nil {
			t.Fatal(err)
		}
		var decoded any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
	}
	var rows []map[string]any
	raw, _ = json.Marshal(desktop)
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	if rows[0]["type"] != "boolean" || rows[0]["current"] != "false" || rows[1]["type"] != "boolean" || rows[1]["current"] != nil || rows[2]["type"] != "select" || rows[2]["current"] != "false" {
		t.Fatalf("Desktop Type/Current contract changed: %s", raw)
	}
	if options[0].Current != "false" {
		t.Fatal("a preference changed the observation")
	}
}
