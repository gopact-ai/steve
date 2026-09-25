package gateway

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/ledger"
)

// claimInput joins live ingress, manual wakes and runtime recovery for one
// durable input. It owns only this process's observer, never durable truth.
func (g *Gateway) claimInput(key string) (func(), bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.durableRunning[key] {
		return nil, false
	}
	if g.durableRunning == nil {
		g.durableRunning = map[string]bool{}
	}
	g.durableRunning[key] = true
	return func() {
		g.mu.Lock()
		delete(g.durableRunning, key)
		g.mu.Unlock()
	}, true
}

// Recovery shares ingress's bounded capacity and never waits for a slot:
// without one, or while its conversation is being served, it leaves that
// input pending for another pass.
func (g *Gateway) claimRecovery(ctx context.Context, receipt ledger.CommandRecord) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var input recoveryInput
	if json.Unmarshal(receipt.Result, &input) != nil || input.ConversationID == "" {
		return nil, errors.New("gateway recovery has no accepted conversation")
	}
	claim, err := g.claimOrdinary(ctx, receipt.ID, input.ConversationID, false)
	if err != nil {
		return nil, err
	}
	return claim.close, nil
}

type inputClaim struct {
	g            *Gateway
	conversation string
	releaseInput func()
}

func (g *Gateway) claimOrdinary(ctx context.Context, key, conversation string, join bool) (*inputClaim, error) {
	release, claimed := g.claimInput(key)
	if !claimed {
		return nil, channel.ErrDeliveryQueued
	}
	if err := g.acquireConversation(ctx, conversation, join); err != nil {
		release()
		return nil, err
	}
	return &inputClaim{g: g, conversation: conversation, releaseInput: release}, nil
}

func (c *inputClaim) close() {
	if c.conversation != "" {
		c.g.releaseConversation(c.conversation)
	}
	c.releaseInput()
}

// A topic receipt changes the route, not the accepted input. Release the old
// conversation reference before nonblocking admission to the proven thread.
// Never wait while retaining a slot needed by the thread's control message.
func (c *inputClaim) move(ctx context.Context, conversation string) error {
	if conversation == c.conversation {
		return nil
	}
	c.g.releaseConversation(c.conversation)
	c.conversation = ""
	if err := c.g.acquireConversation(ctx, conversation, false); err != nil {
		return err
	}
	c.conversation = conversation
	return nil
}

func (g *Gateway) acquireConversation(ctx context.Context, conversation string, join bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	g.mu.Lock()
	if g.serving[conversation] != 0 {
		if join {
			g.serving[conversation]++
			g.mu.Unlock()
			return nil
		}
		g.mu.Unlock()
		return channel.ErrDeliveryQueued
	}
	g.mu.Unlock()
	select {
	case g.slots <- struct{}{}:
	default:
		return channel.ErrDeliveryQueued
	}
	if err := ctx.Err(); err != nil {
		<-g.slots
		return err
	}
	// Recovery reserves the existing conversation slot owner too. Ordinary
	// stop/interrupt input must share that slot, not wait for the very turn
	// it needs to stop. Other recovery inputs stay pending rather than spend
	// all capacity waiting behind the same conversation's native observer.
	g.mu.Lock()
	if g.serving[conversation] != 0 {
		if join {
			g.serving[conversation]++
			g.mu.Unlock()
			<-g.slots
			return nil
		}
		g.mu.Unlock()
		<-g.slots
		return channel.ErrDeliveryQueued
	}
	g.serving[conversation]++
	g.mu.Unlock()
	return nil
}

func (g *Gateway) releaseConversation(conversation string) {
	g.mu.Lock()
	g.serving[conversation]--
	last := g.serving[conversation] == 0
	if last {
		delete(g.serving, conversation)
	}
	g.mu.Unlock()
	if last {
		<-g.slots
	}
}
