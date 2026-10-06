package readmodel

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/gopact-ai/steve/internal/models"
)

func TestBooleanObservedOptionsNeedNoChoiceTable(t *testing.T) {
	book := models.New()
	book.Observe(models.Observation{Harness: "fixture", Selectors: []models.Selector{
		{ID: "toggle", Type: "boolean", Category: "vendor/private", Current: "false"},
		{ID: "unset", Type: "boolean"},
		{ID: "opaque", Type: "select", Current: "false", Values: []string{"false"}},
		{ID: "unlisted", Type: "select", Category: "other/private", Current: "opaque-value"},
		{ID: "model", Type: "select", Category: "model", Current: "m", Values: []string{"m"}},
	}})
	model := New(Sources{Models: book})
	node := &Node{Role: RoleHub, Harnesses: []Harness{{ID: "fixture"}}}
	model.observedModels(node)
	want := []HarnessSelector{
		{ID: "toggle", Type: "boolean", Category: "vendor/private", Current: "false"},
		{ID: "unset", Type: "boolean"},
		{ID: "opaque", Type: "select", Current: "false", Values: []string{"false"}},
		{ID: "unlisted", Type: "select", Category: "other/private", Current: "opaque-value"},
	}
	if !reflect.DeepEqual(node.Harnesses[0].Selectors, want) {
		t.Fatalf("readmodel lost boolean without choices: %+v", node.Harnesses[0])
	}
	raw, err := json.Marshal(node.Harnesses[0])
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip Harness
	if err := json.Unmarshal(raw, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(roundTrip.Selectors, want) {
		t.Fatalf("readmodel JSON lost Type/Current: %s", raw)
	}
}
