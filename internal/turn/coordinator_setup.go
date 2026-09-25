package turn

import (
	"context"
	"fmt"

	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/memory"
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
	// Instructions is the assembled text, exactly as a new session
	// would receive it, and Sections is what it is made of, in order.
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
	capabilities, err := c.assembler.AssembleExtra(selected, mode, c.setupExtras(ctx, conversationID, mode))
	if err != nil {
		return Setup{}, err
	}
	out := Setup{
		Agent: selected.ID, Node: placeLabel(selected.Node), Harness: selected.Harness, Model: selected.Model,
		Mode: string(mode), Instructions: capabilities.Instructions, Sections: capabilities.Sections,
		MCPServers: append([]string{agentmcp.ServerName}, selected.MCPServers...),
	}
	saved := c.store.Conversation(conversationID).Sessions[selected.ID]
	out.Applied = saved.InstructionsApplied && saved.CapabilityHash == capabilities.Fingerprint
	return out, nil
}

// setupExtras is the remembered text a turn would add. The messaging
// server's extras are left out: they mint a session token, which a
// read-only question has no business doing.
func (c *Coordinator) setupExtras(ctx context.Context, conversationID string, mode home.Mode) []capability.Extra {
	if mode != home.ModeOwner {
		return nil
	}
	id := c.memoryProject(ctx, conversationID)
	if id == "" {
		return nil
	}
	text, err := c.memory.Snapshot(ctx, memory.ProjectScope(id))
	if err != nil || text == "" {
		return nil
	}
	return []capability.Extra{{Name: "memory:project:" + id, Memory: text}}
}
