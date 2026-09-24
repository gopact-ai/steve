package turn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/gopact-ai/steve/internal/harness"
)

// ErrConversationBusy is a conversation that cannot be discarded yet
// because a turn of it is still running.
var ErrConversationBusy = errors.New("conversation has a turn in flight")

// DiscardConversation ends a conversation for good. Everything that can
// refuse is asked first — a turn in flight, a task still executing — so
// nothing is destroyed for a delete that will not happen. Then the agent
// sessions it holds are closed on the machines that run them, the tasks
// it opened go with everything delegated from them, the schedules that
// fire into it are dropped, and the records that would resume any of it
// are forgotten.
//
// Sessions close before the tasks are deleted, because a node session is
// authorized by the task it belongs to.
func (c *Coordinator) DiscardConversation(ctx context.Context, conversationID string) error {
	if strings.TrimSpace(conversationID) == "" {
		return errors.New("conversation is required")
	}
	if c.busyWith(conversationID) {
		return fmt.Errorf("%w: %s", ErrConversationBusy, conversationID)
	}
	if err := c.tasks.ChannelIdle(conversationID); err != nil {
		return err
	}
	if err := c.closeConversationSessions(ctx, conversationID); err != nil {
		return err
	}
	if _, err := c.tasks.DeleteChannel(conversationID); err != nil {
		return err
	}
	for _, job := range c.schedules.List(conversationID) {
		if _, _, err := c.schedules.Delete(job.ID); err != nil {
			return fmt.Errorf("drop schedule %s: %w", job.ID, err)
		}
	}
	if err := c.store.DeleteConversation(conversationID); err != nil {
		return err
	}
	c.forgetConversation(conversationID)
	return nil
}

// ResetConversationSessions ends the agent sessions a conversation holds
// without touching the conversation itself, so its next turn opens a
// fresh session. The console uses it when a thread is rewound to an
// edited message: the lines after that message are gone from the
// transcript, and an agent that still remembered them would answer
// questions nobody can see.
func (c *Coordinator) ResetConversationSessions(ctx context.Context, conversationID string) error {
	if strings.TrimSpace(conversationID) == "" {
		return errors.New("conversation is required")
	}
	if c.busyWith(conversationID) {
		return fmt.Errorf("%w: %s", ErrConversationBusy, conversationID)
	}
	return c.closeConversationSessions(ctx, conversationID)
}

// busyWith reports a turn of the conversation running right now, whatever
// agent it belongs to.
func (c *Coordinator) busyWith(conversationID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, entry := range c.cancels {
		if entry != nil && conversationOfKey(key) == conversationID {
			return true
		}
	}
	return false
}

// closeConversationSessions ends the agent processes the conversation
// holds. An archived session was already closed; only the live ones have
// a process to reach.
//
// A machine that cannot be reached is reported and the record goes
// anyway. The alternative is a conversation nobody can ever delete
// because one of its agents ran somewhere that is now offline.
func (c *Coordinator) closeConversationSessions(ctx context.Context, conversationID string) error {
	for agentID, session := range c.store.Conversation(conversationID).Sessions {
		if session.UpstreamID != "" {
			place := harness.Placement{Node: session.NodeID, Harness: session.HarnessID}
			if err := c.runtime.CloseSession(ctx, place, session.UpstreamID); err != nil {
				slog.Error(fmt.Sprintf("turn: close %s session while deleting %s: %v", agentID, conversationID, err), "conversation", conversationID, "agent", agentID, "node", session.NodeID)
			}
		}
		if err := c.store.DeleteSession(conversationID, agentID); err != nil {
			return err
		}
	}
	return nil
}

// forgetConversation drops what this process remembers of a conversation
// outside the stores: how it last reached Steve, and when it was heard.
func (c *Coordinator) forgetConversation(conversationID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.modes, conversationID)
	delete(c.lastSeen, conversationID)
}
