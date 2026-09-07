package turn

import (
	"context"
	"strings"

	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/attempt"
)

type executionGate interface {
	BindExecution(context.Context, agentmcp.Binding, agentmcp.GrantScope) error
}

// bindExecutionGate runs only after the node session identity is committed.
// Handshake metadata may be pending, but no tool call gets an empty-session
// grant. Retained sessions validate the same fixed grant again after handoff.
func (c *Coordinator) bindExecutionGate(ctx context.Context, conversation, id string) error {
	gate, ok := c.gate.(executionGate)
	if !ok {
		return nil
	}
	r, err := c.attempts.Get(ctx, id)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(r.Session, "ns_") || r.Execution == nil {
		return nil
	}
	if saved := c.store.Conversation(conversation).Sessions[r.Agent]; saved.AgentToken == "" {
		return nil
	}
	return gate.BindExecution(ctx, agentmcp.Binding{ConversationID: conversation, AgentID: r.Agent}, agentmcp.GrantScope{TaskID: r.TaskID, TaskEpoch: r.Execution.Epoch, AttemptID: r.ID, ExecutionGeneration: attempt.SessionExecutionEpoch(r), NodeID: r.Node, SessionID: r.Session})
}
