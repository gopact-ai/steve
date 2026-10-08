package turn

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/exec"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/state"
)

// ErrConversationBusy is a conversation that cannot be discarded yet
// because a turn of it is still running.
var ErrConversationBusy = errors.New("conversation has a turn in flight")

// DiscardConversation ends a conversation for good. Everything that can
// refuse is asked first — a turn in flight, a task still executing — so
// nothing is destroyed for a delete that will not happen. Then the agent
// sessions it holds are closed on the machines that run them, the tasks
// it opened go with everything delegated from them, the schedules that
// fire into it are dropped, and its project binding is forgotten. Plan runs
// are retired with the tasks; plan revisions remain historical facts.
//
// Sessions close before the tasks are deleted, because a node session is
// authorized by the task it belongs to.
func (c *Coordinator) DiscardConversation(ctx context.Context, conversationID string) error {
	c.requestMu.RLock()
	defer c.requestMu.RUnlock()
	if c.maintaining {
		return errors.New("conversation retirement is unavailable during maintenance")
	}
	if strings.TrimSpace(conversationID) == "" {
		return errors.New("conversation is required")
	}
	release, err := c.beginConversationRetirement(ctx, conversationID)
	if err != nil {
		return err
	}
	defer release()
	if err := c.tasks.ChannelIdle(ctx, conversationID, checkConversationRetirement); err != nil {
		return err
	}
	if err := c.closeConversationSessions(ctx, conversationID); err != nil {
		return err
	}
	if _, err := c.tasks.DeleteChannelWith(ctx, conversationID, checkConversationRetirement, func(tx *ledger.Tx, ids []string) error {
		if err := exec.DiscardTaskRunsTx(tx, ids); err != nil {
			return err
		}
		return project.DiscardConversationBindingTx(tx, conversationID)
	}); err != nil {
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
	c.requestMu.RLock()
	defer c.requestMu.RUnlock()
	if c.maintaining {
		return errors.New("conversation retirement is unavailable during maintenance")
	}
	if strings.TrimSpace(conversationID) == "" {
		return errors.New("conversation is required")
	}
	release, err := c.beginConversationRetirement(ctx, conversationID)
	if err != nil {
		return err
	}
	defer release()
	// Reset keeps task authority and existing close obligations. Only execution
	// and run retirement facts gate the old context; an already archived close
	// does not prevent another context using a safely settled directory.
	if err := c.tasks.ChannelIdle(ctx, conversationID, checkConversationReset); err != nil {
		return err
	}
	return c.closeConversationSessions(ctx, conversationID)
}

// busyWith reports a turn of the conversation running right now, whatever
// agent it belongs to.
func (c *Coordinator) busyWith(conversationID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.retiring[conversationID] {
		return true
	}
	for key, entry := range c.cancels {
		if entry != nil && conversationOfKey(key) == conversationID {
			return true
		}
	}
	return false
}

// beginConversationRetirement fences every agent slot in this conversation,
// including a newly selected agent, without blocking unrelated conversations.
func (c *coordinatorState) beginConversationRetirement(ctx context.Context, conversationID string) (func(), error) {
	return c.beginSessionRetirement(ctx, conversationID, "")
}

// beginSessionRetirement also serializes history replacement and scheduled
// rotation. An empty agent requires the entire conversation to be idle; a
// specific agent preserves the other agents' already running turns.
func (c *coordinatorState) beginSessionRetirement(ctx context.Context, conversationID, agentID string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.retiring[conversationID] {
		return nil, fmt.Errorf("%w: %s", ErrConversationBusy, conversationID)
	}
	for key, entry := range c.cancels {
		if entry != nil && conversationOfKey(key) == conversationID && (agentID == "" || key == sessionKey(conversationID, agentID)) {
			return nil, fmt.Errorf("%w: %s", ErrConversationBusy, conversationID)
		}
	}
	if c.retiring == nil {
		c.retiring = map[string]bool{}
	}
	c.retiring[conversationID] = true
	return func() { c.mu.Lock(); delete(c.retiring, conversationID); c.mu.Unlock() }, nil
}

func checkConversationReset(tx ledger.Reader, ids []string) error {
	if err := attempt.CheckTaskDeletionTx(tx, ids); err != nil {
		return err
	}
	return exec.CheckTaskRunRetirementTx(tx, ids)
}

// closeConversationSessions retires live contexts after the caller has fenced
// their conversation. Unknown or rejected closes preserve the live slot. A
// certainly undispatched managed close is archived together with its obligation,
// so permanent deletion cannot discard the task authority needed to retry it.
func (c *Coordinator) closeConversationSessions(ctx context.Context, conversationID string) error {
	conversation := c.store.Conversation(conversationID)
	for _, agentID := range slices.Sorted(maps.Keys(conversation.Sessions)) {
		session := conversation.Sessions[agentID]
		var owed *state.OwedClose
		if session.UpstreamID != "" {
			var err error
			owed, err = c.commands().closeSession(ctx, session)
			if err != nil {
				return fmt.Errorf("retire %s session in %s: %w", agentID, conversationID, err)
			}
		}
		at := time.Now().UTC().Format(time.RFC3339Nano)
		if owed != nil {
			owed.OwedAt = at
		}
		if err := c.store.RetireSession(session, at, owed); err != nil {
			return fmt.Errorf("retire %s session in %s: %w", agentID, conversationID, err)
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
