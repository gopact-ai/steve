package turn

import (
	"context"

	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/identity"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/work"
)

type executionGate interface {
	BindExecution(context.Context, agentmcp.Binding, agentmcp.GrantScope) error
}

// bindExecutionGate runs only after the native session identity is committed.
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
	if r.Session == "" || r.Execution == nil {
		return nil
	}
	if saved := c.store.Conversation(conversation).Sessions[r.Agent]; saved.AgentToken == "" {
		return nil
	}
	if accounts := c.owners.Configuration().Accounts; accounts != nil {
		tracked, found := c.tasks.Get(r.TaskID)
		if !found || tracked.Transport == "" || r.By == "" {
			return identity.ErrDenied
		}
		if err := accounts.Book.Update(ctx, func(tx *ledger.Tx) error {
			return work.BindExecutionAccountTx(tx, r.ID, channel.Address{Channel: tracked.Transport, Conversation: tracked.Channel, Message: r.TurnID}, identity.Account{Channel: tracked.Transport, ID: r.By})
		}); err != nil {
			return err
		}
	}
	return gate.BindExecution(ctx, agentmcp.Binding{ConversationID: conversation, AgentID: r.Agent}, agentmcp.GrantScope{TaskID: r.TaskID, TaskEpoch: r.Execution.Epoch, AttemptID: r.ID, ExecutionGeneration: attempt.SessionExecutionEpoch(r), NodeID: r.Node, SessionID: r.Session})
}
