package harness

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/gopact-ai/steve/internal/approval"
	"github.com/gopact-ai/steve/internal/view"
)

// applyApproval puts a session in the mode the owner's fleet-wide approval
// stance asks for. An agent that pins its own mode keeps it: the default is
// what fills the gap, never an override. Each tool names its approval
// levels itself, so the stance is resolved against the modes this agent
// turns out to offer, and an unfamiliar vocabulary leaves it alone.
func applyApproval(ctx context.Context, configurable Configurable, agentID, intent string, pinned map[string]string) bool {
	if intent == "" {
		return false
	}
	option, ok := modeOption(configurable.Settings().Options)
	if !ok {
		return false
	}
	if _, own := pinned[option.ID]; own {
		return false
	}
	modes := make([]approval.Mode, 0, len(option.Choices))
	for _, choice := range option.Choices {
		modes = append(modes, approval.Mode{Value: choice.Value, Label: choice.Label})
	}
	want, ok := approval.Resolve(intent, modes)
	if !ok {
		slog.Info(fmt.Sprintf("harness: agent %q offers no %s approval mode among %d it has", agentID, intent, len(modes)), "agent", agentID, "approval", intent)
		return false
	}
	if want.Value == option.Current {
		return false
	}
	if err := configurable.SetOption(ctx, option.ID, want.Value); err != nil {
		slog.Error(fmt.Sprintf("harness: agent %q set default approval %s (%s): %v", agentID, intent, want.Value, err), "agent", agentID, "approval", intent)
		return false
	}
	slog.Info(fmt.Sprintf("harness: agent %q follows the default approval %s as %q (was %q)", agentID, intent, want.Value, option.Current), "agent", agentID, "approval", intent)
	return true
}

// modeOption is the selector an agent approves with, whatever it calls it:
// ACP reserves the "mode" category for approval behaviour, and an agent
// that predates the category still names the selector "mode".
func modeOption(options []view.Option) (view.Option, bool) {
	for _, option := range options {
		if option.Category == "mode" || option.ID == "mode" {
			return option, true
		}
	}
	return view.Option{}, false
}
