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
	"github.com/gopact-ai/steve/internal/state"
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

// SetPreferences records the owner's choices and applies them. Between
// turns the agent's session is rolled over — the current one archived,
// the next opened with the choices applied — so the task goes on and only
// the upstream session changes.
//
// A turn in flight does not refuse the change either. Every selector an
// agent exposes can be set on a live session, and approval mode is the
// one that has to be: someone tired of approving each command wants the
// asking to stop now, not after this turn. So the running session is
// asked first; live reports whether it took the change. An agent that
// refuses mid-turn falls back to renewal, and the turn finishes on the
// session it started with.
func (c *Coordinator) SetPreferences(ctx context.Context, conversationID, agentID string, patch map[string]string) (bool, error) {
	if c.catalog == nil {
		return false, errors.New("no agent catalog")
	}
	selected, ok := c.catalog.Resolve(agentID)
	if !ok {
		return false, errors.New("no agent " + agentID)
	}
	if err := c.store.SetPreferences(conversationID, selected.ID, patch); err != nil {
		return false, err
	}
	if !c.turnInFlight(conversationID, selected.ID) {
		return false, c.renewSession(ctx, conversationID, selected.ID)
	}
	if c.applyLive(ctx, conversationID, selected.ID, patch) {
		return true, nil
	}
	return false, c.store.SetRenew(conversationID, selected.ID, true)
}

// applyLive sets the owner's choices on the session answering right now.
// It reports true only when every one of them landed: a partial change
// still needs the next session opened fresh, which renewal does.
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
	// it a short while and fall back to renewal rather than hanging.
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

// renewSession ends the agent's upstream session and archives the record,
// so the next turn opens a fresh one with whatever is now preferred. Same
// as a reset's session half, without its task half: the agent's upstream
// session ends, the work it was on does not.
func (c *Coordinator) renewSession(ctx context.Context, conversationID, agentID string) error {
	if saved, ok := c.store.Conversation(conversationID).Sessions[agentID]; ok && saved.UpstreamID != "" {
		if err := c.runtime.CloseSession(ctx, harness.Placement{Node: saved.NodeID, Harness: saved.HarnessID}, saved.UpstreamID); err != nil {
			slog.Error(fmt.Sprintf("turn: close %s session for new preferences: %v", agentID, err), "conversation", conversationID, "agent", agentID, "node", saved.NodeID)
		}
		if err := c.store.ArchiveSession(conversationID, agentID, time.Now().UTC().Format(time.RFC3339)); err != nil {
			return err
		}
	}
	return c.store.SetRenew(conversationID, agentID, false)
}

// renewIfAsked opens the next session fresh when a selector was changed
// while the agent was answering. It runs before a turn or a command takes
// the agent's session, which is the first moment the exchange is safe.
func (c *Coordinator) renewIfAsked(ctx context.Context, conversationID, agentID string) error {
	if !c.store.Conversation(conversationID).Renew[agentID] {
		return nil
	}
	return c.renewSession(ctx, conversationID, agentID)
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
	ctx, cancel := context.WithTimeout(parent, c.timeout)
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
	if err := c.renewIfAsked(ctx, req.ConversationID, selected.ID); err != nil {
		return nil, err
	}
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
