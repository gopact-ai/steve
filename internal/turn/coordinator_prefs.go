package turn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/view"
)

// Preferences are what the owner chose for an agent in a conversation:
// the model under "model", every other selector by its option id. They
// sit over the agent's configured defaults and are applied when the
// native context is opened or resumed.

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

// SetPreferences saves choices for this conversation, not a request to replace
// its native context. A live runner may take them now; otherwise open reapplies
// them on the same context before the next user input.
// A reset selects the current declared default explicitly. Without a declared
// default the owner must choose a value; guessing must not reset native history.
func (c *Coordinator) SetPreferences(ctx context.Context, conversationID, agentID string, patch map[string]string) (bool, error) {
	if c.catalog == nil {
		return false, errors.New("no agent catalog")
	}
	selected, ok := c.catalog.Resolve(agentID)
	if !ok {
		return false, errors.New("no agent " + agentID)
	}
	unlock := c.lockPreferences(conversationID, selected.ID)
	defer unlock()
	previous := c.store.Preferences(conversationID, selected.ID)
	changed := map[string]string{}
	for id, value := range patch {
		if value == "" {
			if previous[id] == "" {
				continue
			}
			value = selected.Options[id]
			if id == "model" {
				value = selected.Model
			}
			if value == "" {
				return false, fmt.Errorf("cannot reset preference %q without a declared default; choose an explicit value", id)
			}
		}
		if previous[id] != value {
			changed[id] = value
		}
	}
	if len(changed) == 0 {
		return false, nil
	}
	if err := c.store.SetPreferences(conversationID, selected.ID, changed); err != nil {
		return false, err
	}
	if !c.turnInFlight(conversationID, selected.ID) {
		return false, nil
	}
	return c.applyLive(ctx, conversationID, selected.ID, changed), nil
}

// Persisting a choice and applying its native RPC must have one ordering.
// Otherwise an older full-access RPC can overwrite a newer read-only choice.
func (c *Coordinator) lockPreferences(conversationID, agentID string) func() {
	lock, _ := c.preferenceLocks.LoadOrStore(sessionKey(conversationID, agentID), &sync.Mutex{})
	mu := lock.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// applyLive sets the owner's choices on the session answering right now.
// It reports true only when every one of them landed: a partial change
// is reapplied and verified before the next prompt on the retained context.
func (c *Coordinator) applyLive(ctx context.Context, conversationID, agentID string, patch map[string]string) bool {
	c.mu.Lock()
	runner := c.active[sessionKey(conversationID, agentID)]
	c.mu.Unlock()
	configurable, ok := runner.(harness.Configurable)
	if !ok {
		return false
	}
	// An agent that is busy answering may not take a selector change
	// until its turn ends, and the owner is waiting on this request. Give
	// it a short while and defer application to the next turn rather than hanging.
	ctx, done := context.WithTimeout(ctx, 10*time.Second)
	defer done()
	for id, value := range patch {
		var err error
		if id == "model" {
			optionID, choices := configurable.ModelChoices()
			if optionID == "" || len(choices) == 0 {
				return false
			}
			err = configurable.SetModel(ctx, optionID, value)
		} else {
			err = configurable.SetOption(ctx, id, value)
		}
		if err != nil {
			slog.Info(fmt.Sprintf("turn: %s did not take %s=%s mid-turn: %v", agentID, id, value, err), "conversation", conversationID, "agent", agentID, "option", id)
			return false
		}
	}
	if reobserver, ok := runner.(harness.Reobserver); ok {
		reobserver.Reobserve()
	}
	return true
}

// Selectors are what the agent's harness offers to choose from, read
// from a throwaway session with the conversation's preferences applied.
// Model choices come first; discovery never resumes a task's session.
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
	// Reading the choices is not a turn: it opens a session of its own,
	// bound to no execution, sends it no work and closes it again. So it
	// does not take the turn slot — the moment the owner most wants to see
	// what else this agent could run is while it is running something. A
	// skills change is different: it moves what every session would load,
	// and the reply would describe an agent that no longer exists.
	if c.skillsUpdating() {
		return Selectors{}, UserError{Text: c.text.T(i18n.TurnBusy, protocol.CommandCancel)}
	}
	req := Request{ConversationID: conversationID, ChatType: protocol.ChatP2P, SenderOpenID: c.ownerOpenID}
	ctx, cancel := context.WithTimeout(parent, c.promptTimeout())
	defer cancel()
	_, workspace, err := c.resolveWorkspace(ctx, req, selected)
	if err != nil {
		return Selectors{}, err
	}
	// A retained node-owned session belongs to an admitted execution.
	// Reading choices must not resume it, inherit an import, or expose
	// the conversation's MCP tools to an unrelated discovery session.
	runner, err := c.open(ctx, state.Session{ConversationID: conversationID}, selected, workspace.Path, nil)
	if err != nil {
		return Selectors{}, UserError{Text: c.text.T(i18n.SelectorsUnavailable, selected.ID, err)}
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := c.runtime.CloseSession(cleanup, placement(selected), runner.ID()); err != nil {
			slog.Error("turn: close selector discovery session", "agent", selected.ID, "node", selected.Node, "error", err)
		}
	}()
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
	return c.open(ctx, saved, selected, workspace.Path, capabilities.MCPServers)
}
