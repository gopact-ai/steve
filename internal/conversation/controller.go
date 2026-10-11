// Package conversation owns conversational context and routing. Execution and
// interaction modules consume it; provider card schemas do not belong here.
package conversation

import (
	"context"
	"errors"
	"fmt"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/state"
	"strings"
	"sync"
	"time"
)

var ErrNoAgent = errors.New("conversation has no configured agent")

type Controller struct {
	Store    *state.Store
	mu       sync.RWMutex
	modes    map[string]home.Mode
	activity map[string]time.Time
}

// Resolve chooses an agent for a query without changing the conversation's
// active agent. A read permission cannot implicitly persist a selection.
func (c *Controller) Resolve(catalog *agent.Catalog, id, input string) (agent.Agent, string, bool, error) {
	if catalog.Default().ID == "" {
		return agent.Agent{}, "", false, ErrNoAgent
	}
	if selection, ok := catalog.Select(input); ok {
		return selection.Agent, selection.Prompt, selection.SwitchOnly, nil
	}
	fields := strings.Fields(strings.TrimSpace(input))
	if len(fields) > 0 && (strings.HasPrefix(fields[0], "@") || fields[0] == string(protocol.CommandUse)) {
		return agent.Agent{}, "", false, fmt.Errorf("unknown agent selection %q", fields[0])
	}
	if selected, ok := catalog.Resolve(c.Store.Conversation(id).ActiveAgent); ok {
		return selected, strings.TrimSpace(input), false, nil
	}
	return catalog.Default(), strings.TrimSpace(input), false, nil
}

func (c *Controller) Select(catalog *agent.Catalog, id, input string) (agent.Agent, string, bool, error) {
	return c.SelectChecked(context.Background(), catalog, id, input, nil)
}

func (c *Controller) SelectChecked(ctx context.Context, catalog *agent.Catalog, id, input string, guard func(*ledger.Tx) error) (agent.Agent, string, bool, error) {
	if catalog.Default().ID == "" {
		return agent.Agent{}, "", false, ErrNoAgent
	}
	if selection, ok := catalog.Select(input); ok {
		if err := c.Store.SetActiveAgentChecked(ctx, id, selection.Agent.ID, guard); err != nil {
			return agent.Agent{}, "", false, err
		}
		return selection.Agent, selection.Prompt, selection.SwitchOnly, nil
	}
	fields := strings.Fields(strings.TrimSpace(input))
	if len(fields) > 0 && (strings.HasPrefix(fields[0], "@") || fields[0] == string(protocol.CommandUse)) {
		return agent.Agent{}, "", false, fmt.Errorf("unknown agent selection %q", fields[0])
	}
	if active := c.Store.Conversation(id).ActiveAgent; active != "" {
		if selected, ok := catalog.Resolve(active); ok {
			return selected, strings.TrimSpace(input), false, nil
		}
	}
	selected := catalog.Default()
	if err := c.Store.SetActiveAgentChecked(ctx, id, selected.ID, guard); err != nil {
		return agent.Agent{}, "", false, err
	}
	return selected, strings.TrimSpace(input), false, nil
}

func (c *Controller) Arrived(id string) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.activity == nil {
		c.activity = map[string]time.Time{}
	}
	now := time.Now()
	c.activity[id] = now
	return now
}
func (c *Controller) HeardSince(id string, mark time.Time) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.activity[id].After(mark)
}
func (c *Controller) RememberMode(id string, mode home.Mode) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.modes == nil {
		c.modes = map[string]home.Mode{}
	}
	c.modes[id] = mode
}
func (c *Controller) Mode(id string) (home.Mode, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	mode, known := c.modes[id]
	return mode, known
}
func (c *Controller) Forget(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.activity, id)
	delete(c.modes, id)
}
