package nodewire

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/gopact-ai/steve/internal/view"
)

func TestBooleanSessionSettingsKeepFalseAndUnsetAcrossNodeWire(t *testing.T) {
	want := []view.Option{
		{ID: "toggle", Type: "boolean", Category: "vendor/private", Current: "false"},
		{ID: "unset", Type: "boolean"},
		{ID: "opaque", Type: "select", Current: "false", Choices: []view.Choice{{Value: "false"}}},
	}
	raw, err := json.Marshal(SessionState{Settings: view.Settings{Options: want}})
	if err != nil {
		t.Fatal(err)
	}
	var state SessionState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(state.Settings.Options, want) {
		t.Fatalf("node wire lost typed settings: %s", raw)
	}
}
