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

// Recovery shares ingress's bounded capacity. Runtime never creates a
// goroutine to wait for a slot; it leaves that input pending for another pass.
func (g *Gateway) claimRecovery(ctx context.Context, receipt ledger.CommandRecord, wait bool) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var input recoveryInput
	if json.Unmarshal(receipt.Result, &input) != nil || input.ConversationID == "" {
		return nil, errors.New("gateway recovery has no accepted conversation")
	}
	release, claimed := g.claimInput(receipt.ID)
	if !claimed {
		return nil, channel.ErrDeliveryQueued
	}
	g.mu.Lock()
	busy := g.serving[input.ConversationID] != 0
	g.mu.Unlock()
	if busy {
		release()
		return nil, channel.ErrDeliveryQueued
	}
	if wait {
		select {
		case g.slots <- struct{}{}:
		case <-ctx.Done():
			release()
			return nil, ctx.Err()
		}
	} else {
		select {
		case g.slots <- struct{}{}:
		default:
			release()
			return nil, channel.ErrDeliveryQueued
		}
	}
	if err := ctx.Err(); err != nil {
		<-g.slots
		release()
		return nil, err
	}
	// Recovery reserves the existing conversation slot owner too. Ordinary
	// stop/interrupt input must share that slot, not wait for the very turn
	// it needs to stop. Other recovery inputs stay pending rather than spend
	// all capacity waiting behind the same conversation's native observer.
	g.mu.Lock()
	if g.serving[input.ConversationID] != 0 {
		g.mu.Unlock()
		<-g.slots
		release()
		return nil, channel.ErrDeliveryQueued
	}
	g.serving[input.ConversationID]++
	g.mu.Unlock()
	return func() {
		g.releaseConversation(input.ConversationID)
		release()
	}, nil
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
