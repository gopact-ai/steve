package node

import (
	"encoding/json"
	"maps"
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

// Only committed/hydrated canonical metadata reaches this copy path. Dynamic
// metadata is normalized once at changed commit ingress, never while polling.
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
	case nil, bool, string, json.Number, float64:
		return v
	default:
		panic("node: metadata must be canonicalized before copying owner state")
	}
}
