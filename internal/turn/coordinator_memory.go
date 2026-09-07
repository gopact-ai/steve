package turn

import (
	"context"
	"errors"
	"log"

	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/memory"
)

// SetMemory wires what Steve remembers.
func (c *Coordinator) SetMemory(svc *memory.Service) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.memory = svc
}

func (c *Coordinator) memoryService() *memory.Service {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.memory
}

// memoryProject is the project whose memory a conversation gets: the
// one it is bound to, or the default, never Steve's own home (its memory
// is the global one).
func (c *Coordinator) memoryProject(ctx context.Context, conversationID string) string {
	attemptID := ""
	if scope, ok := agentmcp.ScopeFromContext(ctx); ok {
		attemptID = scope.AttemptID
	} else if key, ok := execution.KeyOf(ctx); ok {
		attemptID = key.AttemptID
	}
	if attemptID != "" {
		if c.attempts == nil {
			return ""
		}
		r, err := c.attempts.Get(ctx, attemptID)
		if err != nil || r.Project == c.homeProject {
			return ""
		}
		return r.Project
	}
	if c.projects == nil {
		return ""
	}
	id, _, _, err := c.projectFor(ctx, conversationID)
	if err != nil || id == "" || id == c.homeProject {
		return ""
	}
	return id
}

// projectMemory is the extra that carries the bound project's memory
// into the first turn: only for the owner in private, only when the
// project has any.
func (c *Coordinator) projectMemory(ctx context.Context, conversationID string, req Request) []capability.Extra {
	svc := c.memoryService()
	if svc == nil || injectionMode(req.ChatType, req.SenderOpenID, c.ownerOpenID) != home.ModeOwner {
		return nil
	}
	id := c.memoryProject(ctx, conversationID)
	if id == "" {
		return nil
	}
	text, err := svc.Snapshot(ctx, memory.ProjectScope(id))
	if err != nil {
		log.Printf("turn: project %s memory: %v", id, err)
		return nil
	}
	if text == "" {
		return nil
	}
	return []capability.Extra{{Name: "memory:project:" + id, Memory: text}}
}

// memoryScope resolves what an agent said to a scope it may use: the
// owner in private only, and "project" only when the conversation has
// one. A delegated task may read but not write.
func (c *Coordinator) memoryScope(ctx context.Context, conversationID, delegatedBy, raw string, write bool) (*memory.Service, memory.Scope, error) {
	svc := c.memoryService()
	if svc == nil {
		return nil, memory.Scope{}, errors.New("memory is not wired on this gateway")
	}
	if c.modeOf(conversationID) != home.ModeOwner {
		return nil, memory.Scope{}, errors.New("memory is the owner's, in private: this conversation is a group or a guest's, so nothing is remembered or recalled here")
	}
	if write && delegatedBy != "" {
		return nil, memory.Scope{}, errors.New("a delegated task does not write memory; put what is worth keeping in your result and let the parent decide")
	}
	scope, err := memory.ParseScope(raw, c.memoryProject(ctx, conversationID))
	if err != nil {
		return nil, memory.Scope{}, err
	}
	return svc, scope, nil
}

// Remember answers steve_remember.
func (c *Coordinator) Remember(ctx context.Context, conversationID, agentID, delegatedBy, rawScope, section, text, idempotencyKey string) (memory.Receipt, memory.Scope, error) {
	svc, scope, err := c.memoryScope(ctx, conversationID, delegatedBy, rawScope, true)
	if err != nil {
		return memory.Receipt{}, memory.Scope{}, err
	}
	r, err := svc.Remember(ctx, scope, section, text, idempotencyKey, memory.Actor{Conversation: conversationID, Agent: agentID, By: "agent"})
	return r, scope, err
}

// Recall answers steve_recall; an empty scope searches both.
func (c *Coordinator) Recall(ctx context.Context, conversationID, agentID, rawScope, query string, limit int) ([]memory.Hit, string, error) {
	if rawScope != "" {
		svc, scope, err := c.memoryScope(ctx, conversationID, "", rawScope, false)
		if err != nil {
			return nil, "", err
		}
		return svc.Recall(ctx, scope, query, limit)
	}
	svc, global, err := c.memoryScope(ctx, conversationID, "", "global", false)
	if err != nil {
		return nil, "", err
	}
	hits, from, err := svc.Recall(ctx, global, query, limit)
	if err != nil {
		return nil, "", err
	}
	if id := c.memoryProject(ctx, conversationID); id != "" {
		more, _, err := svc.Recall(ctx, memory.ProjectScope(id), query, limit)
		if err != nil {
			return nil, "", err
		}
		hits = append(hits, more...)
	}
	return hits, from, nil
}

// Forget answers steve_forget.
func (c *Coordinator) Forget(ctx context.Context, conversationID, agentID, delegatedBy, rawScope, id string) (memory.Item, error) {
	svc, scope, err := c.memoryScope(ctx, conversationID, delegatedBy, rawScope, true)
	if err != nil {
		return memory.Item{}, err
	}
	return svc.Forget(ctx, scope, id, memory.Actor{Conversation: conversationID, Agent: agentID, By: "agent"})
}
