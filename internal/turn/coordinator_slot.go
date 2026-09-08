// The turn slot: one prompt runs per conversation and agent at a time. A
// new message queues behind or interrupts the running turn, /cancel stops
// it, and the slot bookkeeping is what lets the two agree on who owns the
// session cleanup.

package turn

import (
	"context"
	"errors"
	"fmt"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/i18n"
	"time"
)

// turnEntry tracks one in-flight prompt turn. The blocked prompt goroutine
// owns session cleanup; done is closed by clearActive once it has finished,
// so /cancel can confirm the turn ended before deciding to force-kill.
type turnEntry struct {
	err    error
	cancel context.CancelFunc
	done   chan struct{}
}

func (c *Coordinator) cancel(ctx context.Context, conversationID string, selected agent.Agent) (Result, error) {
	key := sessionKey(conversationID, selected.ID)
	c.mu.Lock()
	runner, entry := c.active[key], c.cancels[key]
	c.mu.Unlock()
	if entry == nil {
		// A turn may be starting right now (the worker already dequeued the
		// message); arm a short-lived flag so a turn that begins within the
		// window is canceled instead of running after the user asked to stop.
		c.mu.Lock()
		c.cancelPending[key] = time.Now().Add(pendingCancelWindow)
		c.mu.Unlock()
		return Result{AgentID: selected.ID, Text: c.text.T(i18n.NoRunningTurn)}, nil
	}
	if runner != nil {
		cancelCtx, stop := context.WithTimeout(ctx, 15*time.Second)
		err := runner.Cancel(cancelCtx)
		stop()
		if err != nil {
			// The agent did not accept the cancel: cancel the turn context
			// and let its owner report whether the session really stopped.
			entry.cancel()
			return Result{}, err
		}
	} else {
		// The turn has begun but has not reached the agent yet
		// (assemble/open/save). Cancel the turn context immediately so
		// it cannot proceed into Prompt after /cancel.
		entry.cancel()
	}
	// Give the agent a chance to stop gracefully; the blocked prompt
	// goroutine owns session cleanup. A timeout reports uncertainty; it
	// cannot safely kill a shared host or claim the writer has stopped.
	select {
	case <-entry.done:
	case <-time.After(10 * time.Second):
		entry.cancel()

		select {
		case <-entry.done:
		case <-time.After(10 * time.Second):
			return Result{}, fmt.Errorf("%w: prompt has not acknowledged cancellation", harness.ErrStopUnconfirmed)
		}
	}
	if errors.Is(entry.err, harness.ErrStopUnconfirmed) {
		return Result{}, entry.err
	}
	return Result{AgentID: selected.ID, Text: c.text.T(i18n.CancelRequested, selected.ID)}, nil
}

// interruptGrace bounds how long a new prompt waits for the turn it is
// replacing to let go. The old turn is already cancelled by then; this only
// covers an agent that is slow to notice.
const interruptGrace = 20 * time.Second

// takeTurn claims the turn slot for a new prompt. By default it waits its
// turn: most new messages add work, and silently killing a running turn to
// make room loses real progress. Interrupting stays one gesture away — a
// "!" prefix (or the card's stop button) cancels the running turn and puts
// the new instruction in its place, which is what steering needs.
func (c *Coordinator) takeTurn(ctx context.Context, conversationID, agentID string, cancel context.CancelFunc, queue bool) bool {
	key := sessionKey(conversationID, agentID)
	for {
		c.mu.Lock()
		// A skills update rewrites what the agent is about to be told, so it
		// still blocks: interrupting would not help, the input is not ready.
		if c.skillsLock > 0 {
			c.mu.Unlock()
			return false
		}
		entry := c.cancels[key]
		if entry == nil {
			c.cancels[key] = &turnEntry{cancel: cancel, done: make(chan struct{})}
			c.mu.Unlock()
			return true
		}
		c.mu.Unlock()
		// Wait without the lock: the running turn releases the slot through
		// clearActive, which needs the same lock to do it.
		if queue {
			// A queued follow-up leaves the running turn alone and simply
			// waits its turn — however long that takes; the running turn's
			// own deadline is the bound.
			select {
			case <-entry.done:
			case <-ctx.Done():
				return false
			}
			continue
		}
		entry.cancel()
		timer := time.NewTimer(interruptGrace)
		select {
		case <-entry.done:
			timer.Stop()
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
			return false
		}
		// clearActive closes done and deletes the entry under one lock, so
		// the next pass sees an empty slot unless another message beat us to
		// it — in which case interrupt that one too.
	}
}

func (c *Coordinator) beginTurn(conversationID, agentID string, cancel context.CancelFunc) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := sessionKey(conversationID, agentID)
	if c.skillsLock > 0 || c.cancels[key] != nil {
		return false
	}
	c.cancels[key] = &turnEntry{cancel: cancel, done: make(chan struct{})}
	return true
}

// pendingCancelWindow is how long an armed cancel stays effective when no
// turn was running yet — long enough to cover the dequeue-to-beginTurn gap.
const pendingCancelWindow = 5 * time.Second

func (c *Coordinator) consumePendingCancel(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	deadline, ok := c.cancelPending[key]
	if !ok {
		return false
	}
	delete(c.cancelPending, key)
	return time.Now().Before(deadline)
}

func (c *Coordinator) setRunner(conversationID, agentID string, runner harness.Runner) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.active[sessionKey(conversationID, agentID)] = runner
}

func (c *Coordinator) clearActive(conversationID, agentID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := sessionKey(conversationID, agentID)
	delete(c.active, key)
	if entry := c.cancels[key]; entry != nil {
		entry.cancel()
		close(entry.done)
	}
	delete(c.cancels, key)
}

func sessionKey(conversationID, agentID string) string { return conversationID + "\x00" + agentID }
