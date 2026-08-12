package turn

import (
	"context"
	"errors"
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

	mu            sync.Mutex
	active        map[string]harness.Runner
	cancels       map[string]*turnEntry
	cancelPending map[string]time.Time
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
		cancelPending: map[string]time.Time{},
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
	if c.consumePendingCancel(sessionKey(conversationID, selected.ID)) {
		return Result{}, context.Canceled
	}
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
		if saved.UpstreamID != "" {
			// The saved upstream session could not be reopened; drop the
			// stale record so the next turn starts a fresh session instead
			// of failing identically on every retry.
			if stateErr := c.store.DeleteSession(conversationID, selected.ID); stateErr != nil {
				log.Printf("turn: delete unreopenable session state: %v", stateErr)
			}
		}
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
		if errors.Is(err, harness.ErrTurnCanceled) {
			// The agent stopped the turn itself (e.g. a permission request
			// was rejected); the session stays consistent, so keep it and
			// clear the taint instead of tearing the process down.
			session.Tainted = false
			session.InstructionsApplied = true
			if stateErr := c.store.SaveSession(session); stateErr != nil {
				log.Printf("turn: save canceled session state: %v", stateErr)
			}
			return Result{}, err
		}
		if ctx.Err() == nil {
			// The turn ended with an error while the context is intact, so
			// the agent stopped by itself and the process is healthy. Drop
			// the session and let the next turn start fresh without killing
			// the shared harness process.
			if stateErr := c.store.DeleteSession(conversationID, selected.ID); stateErr != nil {
				log.Printf("turn: delete failed session state: %v", stateErr)
			}
			return Result{}, err
		}
		// The context expired or was canceled while the turn was possibly
		// still running; the process may be stuck, so terminate it.
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
	c.mu.Lock()
	busy = c.cancels[sessionKey(conversationID, selected.ID)] != nil
	c.mu.Unlock()
	if busy {
		// A turn started while the session was being closed; its error path
		// owns the state cleanup, so leave the record alone.
		return Result{}, fmt.Errorf("agent session already has a running turn")
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
		// A turn may be starting right now (the worker already dequeued the
		// message); arm a short-lived flag so a turn that begins within the
		// window is canceled instead of running after the user asked to stop.
		c.mu.Lock()
		c.cancelPending[key] = time.Now().Add(pendingCancelWindow)
		c.mu.Unlock()
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
	// Give the agent a chance to stop gracefully; the blocked prompt
	// goroutine owns session cleanup. Only abandon the pending call and
	// force-terminate the harness process if the turn does not end in time.
	select {
	case <-entry.done:
	case <-time.After(10 * time.Second):
		entry.cancel()
		if runner != nil {
			runner.Abort()
		}
		select {
		case <-entry.done:
		case <-time.After(10 * time.Second):
			log.Printf("turn: prompt goroutine did not finish after abort")
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
