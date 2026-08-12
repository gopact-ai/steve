package turn

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/state"
)

type runtime interface {
	OpenSession(context.Context, string, string, string, []acp.MCPServer) (harness.Runner, error)
	CloseSession(context.Context, string, string) error
}

type Result struct {
	AgentID  string
	Text     string
	Activity []string
}

type Coordinator struct {
	catalog   *agent.Catalog
	store     *state.Store
	assembler *capability.Assembler
	runtime   runtime
	timeout   time.Duration

	mu      sync.Mutex
	active  map[string]harness.Runner
	cancels map[string]*turnEntry
}

// turnEntry tracks one in-flight prompt turn. The blocked prompt goroutine
// owns session cleanup; done is closed by clearActive once it has finished,
// so /cancel can confirm the turn ended before deciding to force-kill.
type turnEntry struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func New(catalog *agent.Catalog, store *state.Store, assembler *capability.Assembler, runtime runtime, timeout time.Duration) *Coordinator {
	return &Coordinator{
		catalog: catalog, store: store, assembler: assembler, runtime: runtime, timeout: timeout,
		active: map[string]harness.Runner{}, cancels: map[string]*turnEntry{},
	}
}

func (c *Coordinator) Handle(ctx context.Context, conversationID, input string) (Result, error) {
	selected, prompt, switchOnly, err := c.selectAgent(conversationID, input)
	if err != nil {
		return Result{}, err
	}
	if switchOnly {
		return Result{AgentID: selected.ID, Text: "已切换到 " + selected.ID}, nil
	}
	switch strings.TrimSpace(prompt) {
	case "/new", "/clear":
		return c.reset(ctx, conversationID, selected)
	case "/status":
		return c.status(conversationID, selected), nil
	case "/cancel":
		return c.cancel(ctx, conversationID, selected)
	}
	return c.prompt(ctx, conversationID, selected, prompt)
}

func (c *Coordinator) selectAgent(conversationID, input string) (agent.Agent, string, bool, error) {
	if selection, ok := c.catalog.Select(input); ok {
		if err := c.store.SetActiveAgent(conversationID, selection.Agent.ID); err != nil {
			return agent.Agent{}, "", false, err
		}
		return selection.Agent, selection.Prompt, selection.SwitchOnly, nil
	}
	fields := strings.Fields(strings.TrimSpace(input))
	if len(fields) > 0 && (strings.HasPrefix(fields[0], "@") || fields[0] == "/use") {
		return agent.Agent{}, "", false, fmt.Errorf("unknown agent selection %q", fields[0])
	}
	conversation := c.store.Conversation(conversationID)
	if conversation.ActiveAgent != "" {
		if selected, ok := c.catalog.Resolve(conversation.ActiveAgent); ok {
			return selected, strings.TrimSpace(input), false, nil
		}
	}
	selected := c.catalog.Default()
	if err := c.store.SetActiveAgent(conversationID, selected.ID); err != nil {
		return agent.Agent{}, "", false, err
	}
	return selected, strings.TrimSpace(input), false, nil
}

func (c *Coordinator) prompt(parent context.Context, conversationID string, selected agent.Agent, prompt string) (Result, error) {
	ctx, cancel := context.WithTimeout(parent, c.timeout)
	if !c.beginTurn(conversationID, selected.ID, cancel) {
		cancel()
		return Result{}, fmt.Errorf("agent session already has a running turn")
	}
	defer c.clearActive(conversationID, selected.ID)
	capabilities, err := c.assembler.Assemble(selected)
	if err != nil {
		return Result{}, err
	}
	conversation := c.store.Conversation(conversationID)
	saved := conversation.Sessions[selected.ID]
	if saved.Tainted {
		return Result{}, fmt.Errorf("agent session has an uncertain previous turn; use /new before continuing")
	}
	if saved.HarnessID != "" && saved.CapabilityHash != capabilities.Fingerprint {
		return Result{}, fmt.Errorf("agent capabilities changed; use /new before continuing")
	}
	if saved.HarnessID != "" && saved.Workspace != selected.Workspace {
		return Result{}, fmt.Errorf("agent workspace changed; use /new before continuing")
	}
	runner, err := c.open(ctx, saved, selected, capabilities.MCPServers)
	if err != nil {
		return Result{}, err
	}
	session := state.Session{
		ConversationID: conversationID, AgentID: selected.ID, HarnessID: selected.Harness,
		UpstreamID: runner.ID(), Workspace: selected.Workspace, CapabilityHash: capabilities.Fingerprint,
		InstructionsApplied: saved.InstructionsApplied, Tainted: true,
	}
	if err := c.store.SaveSession(session); err != nil {
		c.discard(parent, selected, runner)
		return Result{}, err
	}
	if !session.InstructionsApplied && capabilities.Instructions != "" {
		prompt = capabilities.Instructions + "\n\n" + prompt
	}
	c.setRunner(conversationID, selected.ID, runner)
	out, activity, err := runner.Prompt(ctx, prompt)
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		cancelCtx, stop := context.WithTimeout(context.WithoutCancel(parent), 15*time.Second)
		cancelErr := runner.Cancel(cancelCtx)
		stop()
		runner.Abort()
		if stateErr := c.store.DeleteSession(conversationID, selected.ID); stateErr != nil {
			log.Printf("turn: delete failed session state: %v", stateErr)
		}
		if cancelErr != nil {
			return Result{}, fmt.Errorf("%w; cancel failed: %v", err, cancelErr)
		}
		return Result{}, err
	}
	session.Tainted = false
	session.InstructionsApplied = true
	if err := c.store.SaveSession(session); err != nil {
		log.Printf("turn: save completed session state: %v", err)
		out += "\n\n会话状态保存失败，请使用 /new 后继续。"
	}
	return Result{AgentID: selected.ID, Text: out, Activity: activity}, nil
}

func (c *Coordinator) discard(parent context.Context, selected agent.Agent, runner harness.Runner) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 15*time.Second)
	defer cancel()
	if err := c.runtime.CloseSession(ctx, selected.Harness, runner.ID()); err != nil {
		runner.Abort()
	}
}

func (c *Coordinator) open(ctx context.Context, saved state.Session, selected agent.Agent, servers []acp.MCPServer) (harness.Runner, error) {
	if saved.HarnessID != "" && saved.HarnessID != selected.Harness {
		return nil, fmt.Errorf("session belongs to harness %q, not %q", saved.HarnessID, selected.Harness)
	}
	return c.runtime.OpenSession(ctx, selected.Harness, saved.UpstreamID, selected.Workspace, servers)
}

func (c *Coordinator) reset(ctx context.Context, conversationID string, selected agent.Agent) (Result, error) {
	c.mu.Lock()
	busy := c.cancels[sessionKey(conversationID, selected.ID)] != nil
	c.mu.Unlock()
	if busy {
		return Result{}, fmt.Errorf("agent session already has a running turn")
	}
	session := c.store.Conversation(conversationID).Sessions[selected.ID]
	if err := c.runtime.CloseSession(ctx, session.HarnessID, session.UpstreamID); err != nil {
		return Result{}, err
	}
	if err := c.store.DeleteSession(conversationID, selected.ID); err != nil {
		return Result{}, err
	}
	return Result{AgentID: selected.ID, Text: "已重置 " + selected.ID + " 会话"}, nil
}

func (c *Coordinator) status(conversationID string, selected agent.Agent) Result {
	session := c.store.Conversation(conversationID).Sessions[selected.ID]
	if session.UpstreamID == "" {
		return Result{AgentID: selected.ID, Text: fmt.Sprintf("active=%s harness=%s session=none", selected.ID, selected.Harness)}
	}
	return Result{AgentID: selected.ID, Text: fmt.Sprintf("active=%s harness=%s session=%s", selected.ID, session.HarnessID, session.UpstreamID)}
}

func (c *Coordinator) cancel(ctx context.Context, conversationID string, selected agent.Agent) (Result, error) {
	key := sessionKey(conversationID, selected.ID)
	c.mu.Lock()
	runner, entry := c.active[key], c.cancels[key]
	c.mu.Unlock()
	if entry == nil {
		return Result{AgentID: selected.ID, Text: "当前没有运行中的任务"}, nil
	}
	if runner != nil {
		cancelCtx, stop := context.WithTimeout(ctx, 15*time.Second)
		err := runner.Cancel(cancelCtx)
		stop()
		if err != nil {
			entry.cancel()
			return Result{}, err
		}
	}
	entry.cancel()
	// The blocked prompt goroutine owns session cleanup. Wait for it to
	// finish; only force-terminate the harness process if it does not stop
	// in time, so a turn that completed cleanly is never killed after its
	// state was saved.
	select {
	case <-entry.done:
	case <-time.After(10 * time.Second):
		if runner != nil {
			runner.Abort()
		}
	}
	return Result{AgentID: selected.ID, Text: "已请求取消 " + selected.ID + " 当前任务"}, nil
}

func (c *Coordinator) beginTurn(conversationID, agentID string, cancel context.CancelFunc) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := sessionKey(conversationID, agentID)
	if c.cancels[key] != nil {
		return false
	}
	c.cancels[key] = &turnEntry{cancel: cancel, done: make(chan struct{})}
	return true
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
