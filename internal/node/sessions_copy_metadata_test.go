package node

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/view"
)

type permissionMetadataMap map[string]string
type permissionMetadataSlice []string
type permissionMetadataObject map[string]any
type permissionMetadataStruct struct {
	Items []string
}
type permissionMetadataStructMap struct {
	M map[string]any
}
type permissionMetadataIgnoredCycle struct {
	Items []string
	Self  *permissionMetadataIgnoredCycle `json:"-"`
}

func TestSessionPermissionMetadataSnapshotsOwnContainers(t *testing.T) {
	cases := []struct {
		name  string
		value func() any
	}{
		{"canonical-map", func() any { return map[string]any{"key": "original"} }},
		{"canonical-array", func() any { return []any{"original"} }},
		{"acp-map", func() any { return acp.Meta{"key": "original"} }},
		{"raw-json", func() any { return json.RawMessage(`{"key":"original"}`) }},
		{"bytes", func() any { return []byte("original") }},
		{"typed-map", func() any { return map[string]string{"key": "original"} }},
		{"typed-slice", func() any { return []string{"original"} }},
		{"slice-of-maps", func() any { return []map[string]any{{"key": "original"}} }},
		{"named-map", func() any { return permissionMetadataMap{"key": "original"} }},
		{"named-slice", func() any { return permissionMetadataSlice{"original"} }},
		{"nested-named", func() any {
			return permissionMetadataObject{"key": []permissionMetadataMap{{"key": "original"}}}
		}},
		{"array-of-maps", func() any { return [1]permissionMetadataMap{{"key": "original"}} }},
		{"pointer-to-slice", func() any {
			value := permissionMetadataSlice{"original"}
			return &value
		}},
		{"struct-slice", func() any { return permissionMetadataStruct{Items: []string{"original"}} }},
		{"struct-map", func() any { return permissionMetadataStructMap{M: map[string]any{"key": "original"}} }},
		{"pointer-to-struct-slice", func() any { return &permissionMetadataStruct{Items: []string{"original"}} }},
		{"pointer-to-struct-map", func() any {
			return &permissionMetadataStructMap{M: map[string]any{"key": "original"}}
		}},
		{"struct-ignored-cycle", func() any {
			value := &permissionMetadataIgnoredCycle{Items: []string{"original"}}
			value.Self = value
			return value
		}},
	}
	for _, mode := range []string{"poll", "mutation"} {
		for _, tc := range cases {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				one := progressSession(t)
				next := one.record
				next.State.Questions = []nodewire.SessionQuestion{{
					ID: "metadata-question", CommandID: next.CurrentCommand,
					Question: view.Question{Kind: "permission"},
					Permission: &permission.Ask{Kind: acp.ToolKindOther, Options: []acp.PermissionOption{{
						Kind: acp.PermissionOptionKindAllowOnce, Name: "allow", OptionID: "allow",
						Meta: acp.Meta{"payload": tc.value()},
					}}},
				}}
				if err := one.commitLocked(next); err != nil {
					t.Fatal(err)
				}
				before, err := json.Marshal(one.record)
				if err != nil {
					t.Fatal(err)
				}
				persisted := savedProgress(t, one)
				var state nodewire.SessionState
				if mode == "poll" {
					state = one.state("")
				} else {
					state = one.copyLocked().State
				}
				meta := state.Questions[0].Permission.Options[0].Meta
				meta["payload"] = mutatePermissionMetadata(meta["payload"])
				after, err := json.Marshal(one.record)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(before, after) {
					t.Fatal("returned metadata snapshot changed owner without a durable transition")
				}
				if disk := savedProgress(t, one); !reflect.DeepEqual(disk, persisted) {
					t.Fatal("snapshot mutation changed durable record")
				}
			})
		}
	}
}

// Mutate canonical JSON values, not the programmatic ingress representation.
func mutatePermissionMetadata(value any) any {
	switch value := value.(type) {
	case map[string]any:
		for key, item := range value {
			value[key] = mutatePermissionMetadata(item)
			break
		}
		return value
	case []any:
		if len(value) > 0 {
			value[0] = mutatePermissionMetadata(value[0])
		}
		return value
	case string:
		return "mutated"
	default:
		panic("test fixture has no mutable leaf")
	}
}

func TestSessionPermissionMetadataCopyPreservesValues(t *testing.T) {
	input := acp.Meta{
		"null": nil, "nil-map": permissionMetadataMap(nil), "nil-slice": permissionMetadataSlice(nil),
		"empty-map": permissionMetadataMap{}, "empty-slice": permissionMetadataSlice{},
		"integer": json.Number("9007199254740993"), "nested": permissionMetadataObject{"key": permissionMetadataSlice{"value"}},
		"struct": struct {
			Items  []string
			hidden string
		}{Items: []string{"value"}, hidden: "preserved"},
	}
	canonical, err := canonicalSessionMetadata(input)
	if err != nil {
		t.Fatal(err)
	}
	if copied := copySessionJSON(canonical); !bytes.Equal(metadataJSON(t, input), metadataJSON(t, copied)) {
		t.Fatalf("metadata copy changed JSON-visible values: %#v", copied)
	}
}

func TestSessionPermissionMetadataIgnoredCycleIsNotRetained(t *testing.T) {
	input := &permissionMetadataIgnoredCycle{Items: []string{"original"}}
	input.Self = input
	canonical, err := canonicalSessionMetadata(acp.Meta{"payload": input})
	if err != nil {
		t.Fatal(err)
	}
	copied := copySessionJSON(canonical).(acp.Meta)["payload"].(map[string]any)
	if _, ok := copied["Self"]; ok {
		t.Fatal("JSON-ignored cycle entered owner state")
	}
	copied["Items"].([]any)[0] = "mutated"
	if input.Items[0] != "original" {
		t.Fatal("ignored cycle reaches the original metadata")
	}
}

func TestSessionPermissionMetadataRejectsCyclesBeforeCopy(t *testing.T) {
	cases := []struct {
		name  string
		value func() any
	}{
		{"map", func() any {
			value := map[string]any{}
			value["self"] = value
			return value
		}},
		{"slice", func() any {
			value := []any{nil}
			value[0] = value
			return value
		}},
		{"struct", func() any {
			type cyclic struct{ Self *cyclic }
			value := &cyclic{}
			value.Self = value
			return value
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			one := progressSession(t)
			before := savedProgress(t, one)
			next := one.copyLocked()
			next.State.Questions = []nodewire.SessionQuestion{{
				ID: "cyclic-metadata", CommandID: next.CurrentCommand,
				Permission: &permission.Ask{Options: []acp.PermissionOption{{
					Meta: acp.Meta{"payload": tc.value()},
				}}},
			}}
			err := one.commitLocked(next)
			var unsupported *json.UnsupportedValueError
			if !errors.As(err, &unsupported) {
				t.Fatalf("cyclic metadata must fail explicit JSON validation: %v", err)
			}
			if !reflect.DeepEqual(savedProgress(t, one), before) {
				t.Fatal("invalid metadata changed the durable record")
			}
			if len(one.state("").Questions) != 0 || len(one.copyLocked().State.Questions) != 0 {
				t.Fatal("invalid metadata entered a copyable owner state")
			}
		})
	}
}
