package clientapi

import (
	"bytes"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"time"
)

// SchemaPath is where the generated schema is committed, from the
// repository root.
const SchemaPath = "docs/protocol/steve.client.v1.schema.json"

// wireTypes are the protocol's top-level messages; every type they reach
// is described too.
var wireTypes = []any{
	Initialize{}, ThreadList{}, ThreadDetail{}, CreateThread{}, StartTurn{}, Turn{},
	Interrupt{}, InterruptResult{}, QuestionAnswer{}, AnswerResult{}, AttemptDiff{},
	ProjectList{}, AgentList{}, TaskPage{}, Event{}, Error{},
}

// enums are the closed value sets of the protocol's named string types.
// A client must still accept values it does not know: sets only grow.
var enums = map[reflect.Type][]string{
	reflect.TypeFor[TurnState](): {string(TurnQueued), string(TurnRunning), string(TurnDone), string(TurnFailed), string(TurnCancelled)},
	reflect.TypeFor[ItemKind]():  {string(ItemMessage), string(ItemReasoning), string(ItemToolCall), string(ItemFileChange), string(ItemQuestion), string(ItemMilestone), string(ItemNotice)},
	reflect.TypeFor[EventType](): {string(EventThreadUpdated), string(EventThreadDeleted), string(EventTurnUpdated), string(EventTurnProgress), string(EventItemCompleted),
		string(EventItemRemoved), string(EventQuestionUpdated), string(EventTaskUpdated), string(EventServer)},
}

// Schema is the JSON Schema (draft 2020-12) of every v1 type, generated
// from the Go declarations so the two cannot drift.
func Schema() ([]byte, error) {
	defs := map[string]any{}
	var roots []string
	for _, value := range wireTypes {
		t := reflect.TypeOf(value)
		describe(t, defs)
		roots = append(roots, t.Name())
	}
	sort.Strings(roots)
	refs := make([]any, 0, len(roots))
	for _, name := range roots {
		refs = append(refs, map[string]any{"$ref": "#/$defs/" + name})
	}
	doc := map[string]any{
		"$schema":     "https://json-schema.org/draft/2020-12/schema",
		"$id":         "https://github.com/gopact-ai/steve/" + SchemaPath,
		"title":       Protocol,
		"description": "Messages of the Steve client protocol. Fields are additive only; clients ignore fields and enum values they do not know.",
		"anyOf":       refs,
		"$defs":       defs,
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(doc); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

var timeType = reflect.TypeFor[time.Time]()

// describe returns the schema of t, adding named structs and enums to defs.
func describe(t reflect.Type, defs map[string]any) map[string]any {
	if values, ok := enums[t]; ok {
		if _, done := defs[t.Name()]; !done {
			defs[t.Name()] = map[string]any{"type": "string", "enum": values}
		}
		return map[string]any{"$ref": "#/$defs/" + t.Name()}
	}
	switch {
	case t == timeType:
		return map[string]any{"type": "string", "format": "date-time"}
	case t.Kind() == reflect.Pointer:
		return describe(t.Elem(), defs)
	case t.Kind() == reflect.Slice:
		return map[string]any{"type": "array", "items": describe(t.Elem(), defs)}
	case t.Kind() == reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": describe(t.Elem(), defs)}
	case t.Kind() == reflect.String:
		return map[string]any{"type": "string"}
	case t.Kind() == reflect.Bool:
		return map[string]any{"type": "boolean"}
	case t.Kind() >= reflect.Int && t.Kind() <= reflect.Uint64:
		return map[string]any{"type": "integer"}
	case t.Kind() == reflect.Float32 || t.Kind() == reflect.Float64:
		return map[string]any{"type": "number"}
	case t.Kind() == reflect.Struct:
		ref := map[string]any{"$ref": "#/$defs/" + t.Name()}
		if _, done := defs[t.Name()]; done {
			return ref
		}
		def := map[string]any{"type": "object"}
		defs[t.Name()] = def
		properties := map[string]any{}
		required := []string{}
		for i := range t.NumField() {
			field := t.Field(i)
			tag := field.Tag.Get("json")
			if !field.IsExported() || tag == "-" {
				continue
			}
			name, options, _ := strings.Cut(tag, ",")
			if name == "" {
				name = field.Name
			}
			properties[name] = describe(field.Type, defs)
			optional := strings.Contains(options, "omitempty") || strings.Contains(options, "omitzero")
			if !optional && field.Type.Kind() != reflect.Pointer {
				required = append(required, name)
			}
		}
		def["properties"] = properties
		if len(required) > 0 {
			def["required"] = required
		}
		return ref
	}
	return map[string]any{}
}
