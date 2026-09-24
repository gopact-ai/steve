package turn

import (
	"context"
	"errors"
	"fmt"

	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/state"
)

// prepareCredentials retains native history while replacing a revoked platform
// credential. The old fingerprint has already been verified by prepareSession.
// The node independently verifies the credential-only change against its own
// saved configuration and admits a new execution only after the source settled.
func (t *chatTurn) prepareCredentials(ctx context.Context, saved state.Session, caps capability.Capabilities, extras []capability.Extra, endpoint string) (state.Session, capability.Capabilities, error) {
	c, req, selected := t.c, t.req, t.selected
	if saved.PendingAgentToken == "" {
		_, err := c.gate.PrepareExtras(req.ConversationID, selected.ID, saved.AgentToken, endpoint)
		if err == nil {
			return saved, caps, nil
		}
		if !errors.Is(err, agentmcp.ErrGrantDenied) {
			return saved, caps, err
		}
	}
	if !nodewire.IsManagedSession(saved.UpstreamID) || saved.AgentToken == "" || saved.SessionConfigHash == "" {
		return saved, caps, fmt.Errorf("cannot renew conversation authorization without a verified native context: %w", agentmcp.ErrGrantDenied)
	}
	old := c.gate.DescribeExtras(saved.AgentToken, endpoint)
	if len(old) != 1 || old[0].Name != agentmcp.ServerName || len(extras) == 0 || extras[0].Name != agentmcp.ServerName {
		return saved, caps, errors.New("conversation credential refresh requires the built-in platform MCP")
	}
	if saved.PendingAgentToken == "" {
		token := newAgentToken()
		saved.PendingAgentToken = token
		// Persist the replacement before preparing it or starting a native
		// resume. A crash retries exactly this credential and context.
		if err := c.store.SaveSession(saved); err != nil {
			return saved, caps, err
		}
	}
	next, err := c.gate.PrepareExtras(req.ConversationID, selected.ID, saved.PendingAgentToken, endpoint)
	if err != nil {
		return saved, caps, fmt.Errorf("prepare replacement conversation authorization: %w", err)
	}
	// Replace only the built-in gate extras, preserving memory/instructions.
	if len(next) != 1 || next[0].Name != agentmcp.ServerName {
		return saved, caps, errors.New("conversation credential refresh requires the built-in platform MCP")
	}
	updated := append([]capability.Extra(nil), extras...)
	updated[0] = next[0]
	if saved.PluginRuntime != nil {
		mode := injectionMode(req.ChatType, req.SenderOpenID, c.ownerOpenID)
		caps, err = c.assembler.AssembleExtraPinned(selected, mode, updated, saved.PluginSkillsFingerprint)
	} else {
		caps, err = c.assemble(selected, req, updated)
	}
	if err != nil {
		return saved, caps, err
	}
	t.credentialRefresh = &nodewire.MCPAuthorizationRefresh{PreviousAuthorization: "Bearer " + saved.AgentToken}
	saved.AgentToken, saved.PendingAgentToken = saved.PendingAgentToken, ""
	return saved, caps, nil
}
