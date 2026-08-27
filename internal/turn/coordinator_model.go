package turn

import (
	"context"
	"fmt"
	"strings"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/view"
)

// modelCmd shows or changes the model behind the current agent. It opens the
// session the same way a prompt does, because the model selector is a
// property of a live session: the agent only reports its options once one is
// open, and a change has to land on the session the next turn will use.
func (c *Coordinator) modelCmd(parent context.Context, req Request, selected agent.Agent, want string) (Result, error) {
	conversationID := req.ConversationID
	ctx, cancel := context.WithTimeout(parent, c.timeout)
	// Take the turn lock: switching model under a running turn would change
	// the agent out from under it.
	if !c.beginTurn(conversationID, selected.ID, cancel) {
		cancel()
		return Result{}, UserError{Text: c.text.T(i18n.TurnBusy, protocol.CommandCancel)}
	}
	defer c.clearActive(conversationID, selected.ID)

	runner, err := c.openForCommand(ctx, req, selected)
	if err != nil {
		return Result{}, err
	}
	configurable, ok := runner.(harness.Configurable)
	if !ok {
		return Result{}, UserError{Text: c.text.T(i18n.ModelUnsupported, selected.ID)}
	}
	optionID, choices := configurable.ModelChoices()
	if optionID == "" || len(choices) == 0 {
		return Result{}, UserError{Text: c.text.T(i18n.ModelUnsupported, selected.ID)}
	}
	current := configurable.Settings().Model
	if strings.TrimSpace(want) == "" {
		return Result{AgentID: selected.ID, Text: c.text.T(
			i18n.ModelCurrent, current, modelList(choices, current), protocol.CommandModel)}, nil
	}
	picked, err := matchModel(choices, want)
	if err != nil {
		return Result{}, c.modelMatchError(err, choices, want)
	}
	if err := configurable.SetModel(ctx, optionID, picked.Value); err != nil {
		return Result{}, err
	}
	// Read back rather than echoing the request: the agent confirms with a
	// config option update, and it is the authority on what it now runs.
	settled := configurable.Settings().Model
	if settled == "" {
		settled = picked.Label
	}
	return Result{AgentID: selected.ID, Text: c.text.T(i18n.ModelSwitched, settled)}, nil
}

// openForCommand gets the conversation's session without any of the
// turn-taking a prompt does: no task budget is spent and nothing is marked
// tainted, because a command that only reads or sets a selector is not a
// turn.
func (c *Coordinator) openForCommand(ctx context.Context, req Request, selected agent.Agent) (harness.Runner, error) {
	capabilities, err := c.assemble(selected, req, nil)
	if err != nil {
		return nil, err
	}
	saved := c.store.Conversation(req.ConversationID).Sessions[selected.ID]
	workspace := c.sessionWorkspace(req, selected, saved)
	runner, err := c.open(ctx, saved, selected, workspace, capabilities.MCPServers)
	if err != nil && saved.UpstreamID != "" {
		// Same fallback as a prompt: a session the agent no longer holds is
		// replaced rather than reported as a failure.
		saved.UpstreamID = ""
		saved.InstructionsApplied = false
		runner, err = c.open(ctx, saved, selected, workspace, capabilities.MCPServers)
	}
	return runner, err
}

var errModelUnknown = fmt.Errorf("no such model")
var errModelAmbiguous = fmt.Errorf("several models match")

// matchModel resolves what a person typed. An exact id or label wins
// outright; otherwise a unique case-insensitive substring does, so "sol" or
// "opus" is enough without pasting "gpt-5.6-sol".
func matchModel(choices []view.Choice, want string) (view.Choice, error) {
	want = strings.TrimSpace(want)
	folded := strings.ToLower(want)
	for _, choice := range choices {
		if choice.Value == want || strings.EqualFold(choice.Label, want) {
			return choice, nil
		}
	}
	var hits []view.Choice
	for _, choice := range choices {
		if strings.Contains(strings.ToLower(choice.Value), folded) ||
			strings.Contains(strings.ToLower(choice.Label), folded) {
			hits = append(hits, choice)
		}
	}
	switch len(hits) {
	case 0:
		return view.Choice{}, errModelUnknown
	case 1:
		return hits[0], nil
	default:
		return view.Choice{}, errModelAmbiguous
	}
}

func (c *Coordinator) modelMatchError(err error, choices []view.Choice, want string) error {
	if err == errModelAmbiguous {
		var hits []string
		folded := strings.ToLower(want)
		for _, choice := range choices {
			if strings.Contains(strings.ToLower(choice.Value), folded) ||
				strings.Contains(strings.ToLower(choice.Label), folded) {
				hits = append(hits, choice.Label)
			}
		}
		return UserError{Text: c.text.T(i18n.ModelAmbiguous, want, strings.Join(hits, ", "))}
	}
	return UserError{Text: c.text.T(i18n.ModelUnknown, want)}
}

// modelList renders the options with the live one marked. Agents can offer
// dozens, so this caps the list and says so rather than filling the chat.
func modelList(choices []view.Choice, current string) string {
	const maxListed = 12
	lines := make([]string, 0, maxListed+1)
	for i, choice := range choices {
		if i == maxListed {
			lines = append(lines, fmt.Sprintf("… +%d", len(choices)-maxListed))
			break
		}
		mark := "  "
		if choice.Label == current {
			mark = "▸ "
		}
		lines = append(lines, mark+choice.Label)
	}
	return strings.Join(lines, "\n")
}
