package turn

import (
	"context"
	"fmt"

	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/memory"
	"github.com/gopact-ai/steve/internal/state"
)

// Setup is what an agent is given to work with in a conversation: the
// instructions it reads when its session opens, what those instructions
// are made of, and the servers it is connected to. A trace answers what
// happened in one turn; this answers what the agent had in hand, which
// is the question people actually ask when a reply looks wrong.
type Setup struct {
	Agent, Node, Harness, Model string
	// Mode is which identity the home lends this conversation.
	Mode string
	// Instructions is the assembled text a session is given, and Sections
	// is what it is made of, in order. The messaging server's guidance is
	// included only for a session that already holds a token for it. A
	// conversation without one is shown the rest, although the session its
	// next turn opens is also given that guidance when this hub runs the
	// messaging server and the agent speaks HTTP MCP.
	Instructions string
	Sections     []capability.Section
	MCPServers   []string
	// Applied says the current session already read these instructions.
	// When false the next turn will send them — a new session, or one
	// whose context changed.
	Applied bool
}

// SessionSetup assembles the agent's standing contract for a
// conversation. It reads the same pieces a turn does — home identity,
// the agent's prompt and skills, project memory — but starts nothing
// and changes nothing, so the page can ask for it at any moment.
func (c *Coordinator) SessionSetup(ctx context.Context, conversationID, agentID string) (Setup, error) {
	if agentID == "" {
		agentID = c.store.Conversation(conversationID).ActiveAgent
		if agentID == "" {
			agentID = c.catalog.Default().ID
		}
	}
	selected, ok := c.catalog.Resolve(agentID)
	if !ok {
		return Setup{}, fmt.Errorf("no agent %q", agentID)
	}
	mode := c.modeOf(conversationID)
	saved := c.store.Conversation(conversationID).Sessions[selected.ID]
	capabilities, err := c.assembler.AssembleExtra(selected, mode, c.setupExtras(ctx, conversationID, mode, saved))
	if err != nil {
		return Setup{}, err
	}
	out := Setup{
		Agent: selected.ID, Node: placeLabel(selected.Node), Harness: selected.Harness, Model: selected.Model,
		Mode: string(mode), Instructions: capabilities.Instructions, Sections: capabilities.Sections,
		MCPServers: append([]string{agentmcp.ServerName}, selected.MCPServers...),
	}
	out.Applied = saved.InstructionsApplied && saved.CapabilityHash == capabilities.Fingerprint
	return out, nil
}

// setupExtras is what a turn would add on top of the agent's own set: the
// messaging server the session already holds, then remembered text. A
// session holds the messaging server exactly when it keeps a token for
// it. A read-only question neither mints a token nor asks the agent's
// machine where it listens, so the server is described at the hub's own
// address; a fingerprint cannot tell that from a node's loopback, since
// both are this server on 127.0.0.1 and fingerprints leave out its port.
func (c *Coordinator) setupExtras(ctx context.Context, conversationID string, mode home.Mode, saved state.Session) []capability.Extra {
	var extras []capability.Extra
	if c.gate != nil && saved.AgentToken != "" {
		extras = c.gate.DescribeExtras(saved.AgentToken, "")
	}
	if mode != home.ModeOwner {
		return extras
	}
	id := c.memoryProject(ctx, conversationID)
	if id == "" {
		return extras
	}
	text, err := c.memory.Snapshot(ctx, memory.ProjectScope(id))
	if err != nil || text == "" {
		return extras
	}
	return append(extras, capability.Extra{Name: "memory:project:" + id, Memory: text})
}
