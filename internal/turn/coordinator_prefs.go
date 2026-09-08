package turn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/view"
)

// Preferences are what the owner chose for an agent in a conversation:
// the model under "model", every other selector by its option id. They
// sit over the agent's configured defaults and are applied when a fresh
// session opens.

// preferred merges the conversation's choices over the agent's
// configuration: what to pass to ApplyPreferences.
func (c *Coordinator) preferred(conversationID string, selected agent.Agent) (string, map[string]string) {
	model := selected.Model
	options := map[string]string{}
	for k, v := range selected.Options {
		options[k] = v
	}
	for k, v := range c.store.Preferences(conversationID, selected.ID) {
		if k == "model" {
			model = v
			continue
		}
		options[k] = v
	}
	return model, options
}

// Preferences answers the page: what is chosen for this agent here.
func (c *Coordinator) Preferences(conversationID, agentID string) map[string]string {
	return c.store.Preferences(conversationID, agentID)
}

// SetPreferences records the owner's choices and rolls the agent's
// session over — the current one is archived, the next turn opens a
// fresh one with the choices applied. The task goes on; only the
// upstream session changes. Refused while a turn runs.
func (c *Coordinator) SetPreferences(ctx context.Context, conversationID, agentID string, patch map[string]string) error {
	if c.catalog == nil {
		return errors.New("no agent catalog")
	}
	selected, ok := c.catalog.Resolve(agentID)
	if !ok {
		return errors.New("no agent " + agentID)
	}
	if c.isActive(conversationID, selected.ID) {
		return UserError{Text: c.text.T(i18n.TurnBusy, protocol.CommandCancel)}
	}
	if err := c.store.SetPreferences(conversationID, selected.ID, patch); err != nil {
		return err
	}
	if saved, ok := c.store.Conversation(conversationID).Sessions[selected.ID]; ok && saved.UpstreamID != "" {
		// Same as a reset's session half, without its task half: the
		// agent's upstream session ends, the work it was on does not.
		if err := c.runtime.CloseSession(ctx, harness.Placement{Node: saved.NodeID, Harness: saved.HarnessID}, saved.UpstreamID); err != nil {
			slog.Error(fmt.Sprintf("turn: close %s session for new preferences: %v", selected.ID, err), "conversation", conversationID, "agent", selected.ID, "node", saved.NodeID)
		}
		if err := c.store.ArchiveSession(conversationID, selected.ID, time.Now().UTC().Format(time.RFC3339)); err != nil {
			return err
		}
	}
	return nil
}

// Selectors are what the agent's harness offers to choose from, read
// from a live session — opened for the purpose when there is none —
// with what is currently set. Model choices come first.
type Selectors struct {
	Model   string        `json:"model,omitempty"`
	Models  []view.Choice `json:"models,omitempty"`
	Options []view.Option `json:"options,omitempty"`
}

func (c *Coordinator) Selectors(parent context.Context, conversationID, agentID string) (Selectors, error) {
	if c.catalog == nil {
		return Selectors{}, errors.New("no agent catalog")
	}
	selected, ok := c.catalog.Resolve(agentID)
	if !ok {
		return Selectors{}, errors.New("no agent " + agentID)
	}
	req := Request{ConversationID: conversationID, ChatType: protocol.ChatP2P, SenderOpenID: c.ownerOpenID}
	ctx, cancel := context.WithTimeout(parent, c.timeout)
	if !c.beginTurn(conversationID, selected.ID, cancel) {
		cancel()
		return Selectors{}, UserError{Text: c.text.T(i18n.TurnBusy, protocol.CommandCancel)}
	}
	defer c.clearActive(conversationID, selected.ID)
	runner, err := c.openForCommand(ctx, req, selected)
	if err != nil {
		return Selectors{}, err
	}
	configurable, ok := runner.(harness.Configurable)
	if !ok {
		return Selectors{}, nil
	}
	out := Selectors{}
	if _, choices := configurable.ModelChoices(); len(choices) > 0 {
		out.Models = choices
	}
	settings := configurable.Settings()
	out.Model = settings.Model
	for _, o := range settings.Options {
		if o.Category == "model" {
			continue
		}
		out.Options = append(out.Options, o)
	}
	return out, nil
}

// ProjectOf is the project a conversation works in, "" when none.
func (c *Coordinator) ProjectOf(ctx context.Context, conversationID string) string {
	if c.projects == nil {
		return ""
	}
	id, _, _, err := c.projectFor(ctx, conversationID)
	if err != nil {
		return ""
	}
	return id
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
	saved.ConversationID = req.ConversationID
	_, workspace, err := c.resolveWorkspace(ctx, req, selected)
	if err != nil {
		return nil, err
	}
	runner, err := c.open(ctx, saved, selected, workspace.Path, capabilities.MCPServers)
	if err != nil && saved.UpstreamID != "" && !strings.HasPrefix(saved.UpstreamID, "ns_") {
		// Same fallback as a prompt: a session the agent no longer holds is
		// replaced rather than reported as a failure.
		saved.UpstreamID = ""
		saved.InstructionsApplied = false
		runner, err = c.open(ctx, saved, selected, workspace.Path, capabilities.MCPServers)
	}
	return runner, err
}
