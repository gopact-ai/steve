package node

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/view"
)

// copyLocked is a mutation snapshot. Protocol validation belongs at ingress,
// not in a JSON decoder that can silently drop fields while making a copy.
func (one *ownedSession) copyLocked() sessionRecord {
	next := one.record
	next.State = copySessionState(next.State)
	next.CommandHashes = maps.Clone(next.CommandHashes)
	next.Commands = maps.Clone(next.Commands)
	for id, command := range next.Commands {
		command.Activity = slices.Clone(command.Activity)
		next.Commands[id] = command
	}
	return next
}

// A poll owns its state and selected command, not the session's receipt journal.
func copySessionState(state nodewire.SessionState) nodewire.SessionState {
	state.NativeImport = state.NativeImport.Clone()
	state.Plugin = state.Plugin.Clone()
	if state.OpenReceipt != nil {
		receipt := *state.OpenReceipt
		state.OpenReceipt = &receipt
	}
	state.Settings = copySessionSettings(state.Settings)
	state.ModelChoices = slices.Clone(state.ModelChoices)
	state.Progress = copySessionProgress(state.Progress)
	if state.Command != nil {
		command := *state.Command
		command.Activity = slices.Clone(command.Activity)
		state.Command = &command
	}
	state.Questions = slices.Clone(state.Questions)
	for i := range state.Questions {
		question := &state.Questions[i]
		question.Question.Choices = slices.Clone(question.Question.Choices)
		if question.Answer != nil {
			answer := *question.Answer
			question.Answer = &answer
		}
		if question.Permission != nil {
			ask := *question.Permission
			ask.Options = slices.Clone(ask.Options)
			for j := range ask.Options {
				if ask.Options[j].Meta != nil {
					ask.Options[j].Meta = copySessionJSON(ask.Options[j].Meta).(acp.Meta)
				}
			}
			question.Permission = &ask
		}
	}
	return state
}

func copySessionSettings(settings view.Settings) view.Settings {
	settings.Models = slices.Clone(settings.Models)
	settings.Options = slices.Clone(settings.Options)
	for i := range settings.Options {
		settings.Options[i].Choices = slices.Clone(settings.Options[i].Choices)
	}
	return settings
}

func copySessionProgress(progress view.Progress) view.Progress {
	progress.Tools = copySessionTools(progress.Tools)
	progress.Settings = copySessionSettings(progress.Settings)
	progress.Plan = slices.Clone(progress.Plan)
	progress.Timeline = slices.Clone(progress.Timeline)
	if progress.Usage.Cost != nil {
		cost := *progress.Usage.Cost
		progress.Usage.Cost = &cost
	}
	return progress
}

func copySessionTools(tools []view.Tool) []view.Tool {
	tools = slices.Clone(tools)
	for i := range tools {
		tools[i].Children = copySessionTools(tools[i].Children)
	}
	return tools
}

// Wire ACP metadata takes the canonical map/array fast path. Programmatic
// metadata may also contain typed containers or structs with exported fields.
// JSON validation happens at commit, before metadata becomes copyable state.
func copySessionJSON(value any) any {
	switch v := value.(type) {
	case acp.Meta:
		out := maps.Clone(v)
		for key, value := range out {
			out[key] = copySessionJSON(value)
		}
		return out
	case map[string]any:
		out := maps.Clone(v)
		for key, value := range out {
			out[key] = copySessionJSON(value)
		}
		return out
	case []any:
		out := slices.Clone(v)
		for i := range out {
			out[i] = copySessionJSON(out[i])
		}
		return out
	case json.RawMessage:
		return slices.Clone(v)
	case []byte:
		return slices.Clone(v)
	default:
		if value == nil {
			return nil
		}
		return copySessionMetadataValue(reflect.ValueOf(value), make(map[sessionMetadataVisit]reflect.Value)).Interface()
	}
}

type sessionMetadataVisit struct {
	typ     reflect.Type
	pointer uintptr
	length  int
}

// Programmatic ACP metadata can contain named or typed JSON containers, not
// just the map[string]any/[]any produced by wire decoding. Keep reflection
// confined to this metadata fallback; live state and receipts stay typed.
// Private fields stay unchanged: this is not a clone of opaque custom
// MarshalJSON implementations. Memoization also bounds traversal of cycles
// that JSON legitimately ignores (for example, an exported json:"-" field).
func copySessionMetadataValue(value reflect.Value, seen map[sessionMetadataVisit]reflect.Value) reflect.Value {
	var visit sessionMetadataVisit
	switch value.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice:
		if value.IsNil() {
			return value
		}
		visit = sessionMetadataVisit{typ: value.Type(), pointer: value.Pointer()}
		if value.Kind() == reflect.Slice {
			visit.length = value.Len()
		}
		if out, ok := seen[visit]; ok {
			return out
		}
	}
	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return value
		}
		out := reflect.New(value.Type()).Elem()
		out.Set(copySessionMetadataValue(value.Elem(), seen))
		return out
	case reflect.Pointer:
		out := reflect.New(value.Type().Elem()).Convert(value.Type())
		seen[visit] = out
		out.Elem().Set(copySessionMetadataValue(value.Elem(), seen))
		return out
	case reflect.Map:
		out := reflect.MakeMapWithSize(value.Type(), value.Len())
		seen[visit] = out
		for entries := value.MapRange(); entries.Next(); {
			out.SetMapIndex(entries.Key(), copySessionMetadataValue(entries.Value(), seen))
		}
		return out
	case reflect.Slice, reflect.Array:
		var out reflect.Value
		if value.Kind() == reflect.Slice {
			out = reflect.MakeSlice(value.Type(), value.Len(), value.Len())
			seen[visit] = out
		} else {
			out = reflect.New(value.Type()).Elem()
		}
		for i := 0; i < value.Len(); i++ {
			out.Index(i).Set(copySessionMetadataValue(value.Index(i), seen))
		}
		return out
	case reflect.Struct:
		out := reflect.New(value.Type()).Elem()
		out.Set(value)
		for i := 0; i < value.NumField(); i++ {
			if value.Type().Field(i).IsExported() {
				out.Field(i).Set(copySessionMetadataValue(value.Field(i), seen))
			}
		}
		return out
	default:
		return value
	}
}
