package models

import (
	"reflect"
	"testing"

	"github.com/gopact-ai/steve/internal/view"
)

func TestBooleanObservationPreservesTypeCurrentAndOpaqueCategory(t *testing.T) {
	options := []view.Option{
		{ID: "toggle", Name: "Toggle", Type: "boolean", Category: "vendor/private", Current: "false"},
		{ID: "unset", Name: "Unset", Type: "boolean"},
		{ID: "opaque", Name: "Opaque", Type: "select", Current: "false", Choices: []view.Choice{{Value: "false", Label: "Literal false"}}},
	}
	book := New()
	doc := &memDoc{}
	if err := book.Persist(doc); err != nil {
		t.Fatal(err)
	}
	obs, err := NewProber(stubOpener{runner: &reportingRunner{settings: view.Settings{Options: options}}}, book, nil).Probe(t.Context(), Endpoint{Node: "node", Harness: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	want := []Selector{
		{ID: "toggle", Name: "Toggle", Type: "boolean", Category: "vendor/private", Current: "false"},
		{ID: "unset", Name: "Unset", Type: "boolean"},
		{ID: "opaque", Name: "Opaque", Type: "select", Current: "false", Choices: []string{"Literal false"}, Values: []string{"false"}},
	}
	if !reflect.DeepEqual(obs.Selectors, want) {
		t.Fatalf("probe projection=%+v, want %+v", obs.Selectors, want)
	}
	reopened := New()
	if err := reopened.Persist(doc); err != nil {
		t.Fatal(err)
	}
	saved, ok := reopened.Get("node", "fixture")
	if !ok || !reflect.DeepEqual(saved.Selectors, want) {
		t.Fatalf("persisted observation lost false/unset: %+v", saved)
	}
}
