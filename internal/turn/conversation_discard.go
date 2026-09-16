package turn

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/gopact-ai/steve/internal/harness"
)

// ErrConversationBusy is a conversation that cannot be discarded yet
// because a turn of it is still running.
var ErrConversationBusy = errors.New("conversation has a turn in flight")

// DiscardConversation ends a conversation for good. The tasks it opened
// go first — with everything delegated from them, and only if none is
// executing — then every agent session it holds is closed on the machine
// that runs it, the schedules that fire into it are dropped, and the
// records that would resume any of it are forgotten.
//
// Each step is safe to repeat: a discard interrupted by an unreachable
// machine finishes when it is called again.
func (c *Coordinator) DiscardConversation(ctx context.Context, conversationID string) error {
	if strings.TrimSpace(conversationID) == "" {
		return errors.New("conversation is required")
	}
	if c.busyWith(conversationID) {
		return fmt.Errorf("%w: %s", ErrConversationBusy, conversationID)
	}
	if c.tasks != nil {
		if _, err := c.tasks.DeleteChannel(conversationID); err != nil {
			return err
		}
	}
	if err := c.closeConversationSessions(ctx, conversationID); err != nil {
		return err
	}
	if c.schedules != nil {
		for _, job := range c.schedules.List(conversationID) {
			if _, _, err := c.schedules.Delete(job.ID); err != nil {
				return fmt.Errorf("drop schedule %s: %w", job.ID, err)
			}
		}
	}
	if c.store != nil {
		if err := c.store.DeleteConversation(conversationID); err != nil {
			return err
		}
	}
	c.forgetConversation(conversationID)
	return nil
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
func (c *Coordinator) closeConversationSessions(ctx context.Context, conversationID string) error {
	if c.store == nil || c.runtime == nil {
		return nil
	}
	for agentID, session := range c.store.Conversation(conversationID).Sessions {
		if session.UpstreamID == "" {
			continue
		}
		place := harness.Placement{Node: session.NodeID, Harness: session.HarnessID}
		if err := c.runtime.CloseSession(ctx, place, session.UpstreamID); err != nil {
			return fmt.Errorf("close %s session of %s: %w", agentID, conversationID, err)
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
