// The prompt path: one message becomes one leased attempt through
// lifecycle.Run, with the platform work around the agent (gate, capability
// assembly, session opening) and the spend it charges the task.

package turn

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/idle"
	"github.com/gopact-ai/steve/internal/lifecycle"
	"github.com/gopact-ai/steve/internal/onboard"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/view"
	"log/slog"
	"sync"
	"time"
)

func (c *Coordinator) prompt(parent context.Context, req Request, selected agent.Agent, prompt string) (result Result, err error) {
	conversationID := req.ConversationID
	turnCtx, cancel := context.WithCancel(parent)
	if !c.takeTurn(turnCtx, conversationID, selected.ID, cancel, req.Queue) {
		cancel()
		if c.skillsUpdating() {
			return Result{}, UserError{Text: c.text.T(i18n.SkillsUpdating)}
		}
		return Result{}, UserError{Text: c.text.T(i18n.TurnBusy, protocol.CommandCancel)}
	}
	defer c.clearActive(conversationID, selected.ID)
	req.phase(view.PhaseWaking)
	clock := newTurnClock()
	defer func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if entry := c.cancels[sessionKey(conversationID, selected.ID)]; entry != nil {
			entry.err = err
		}
	}()
	// The clock starts now, not at arrival: a queued prompt must not
	// burn its own running time standing behind the turn it waited for.
	// And it is an idle clock: it runs out after c.timeout of silence,
	// not of work, so a turn that awaits other agents is not cut short
	// while they are still answering.
	idleCtx, expire, touch := idle.WithTimeout(turnCtx, c.timeout)
	var ctx context.Context = idleCtx
	defer expire()
	if c.RegisterIdle != nil {
		defer c.RegisterIdle(selected.Node, idleCtx)()
	}
	if c.consumePendingCancel(sessionKey(conversationID, selected.ID)) {
		return Result{}, context.Canceled
	}
	t := &chatTurn{c: c, req: req, selected: selected, clock: clock, prompt: prompt}
	if c.executions != nil {
		taskID := ""
		if c.tasks != nil {
			if previous, ok := c.tasks.Active(req.ConversationID, selected.ID, req.Origin); ok {
				taskID = previous.ID
			}
		}
		scope, scopeErr := c.executions.Begin(ctx, execution.Key{TaskID: taskID, InstanceID: req.MessageID})
		if scopeErr != nil {
			return Result{}, scopeErr
		}
		t.scope = scope
		defer func() { scope.Finish(t.unresolved(err)) }()
		ctx = scope.Context()
	}
	// The directory is settled before the task opens: a turn that has
	// nowhere to run has not started and spends nothing.
	binding, workspace, err := c.resolveWorkspace(ctx, req, selected)
	if err != nil {
		return Result{}, err
	}
	// The task opens only after the turn lock is held, so a rejected or
	// cancelled turn never spends a turn from the budget.
	tracked, taskErr := c.beginTask(req, selected, prompt, binding, workspace.Path)
	if taskErr != nil {
		return Result{}, taskErr
	}
	// Timed from here, not from arrival: a queued prompt's wait is not the
	// agent's running time, and the reminder is about how long the work took.
	started := time.Now()
	// Progress is stamped with the agent the turn runs as, and the last
	// report's usage is what the task's attempt is charged.
	spent := &turnSpend{touch: touch}
	req.OnProgress = spent.wrap(req.OnProgress, selected.ID)
	t.req, t.spent, t.tracked, t.binding, t.workspace = req, spent, tracked, binding, workspace
	if tracked != "" {
		defer func() {
			if t.managed && errors.Is(err, harness.ErrStopUnconfirmed) {
				return
			}
			c.finishTask(tracked, err, spent.tokens(), spent.model())
			c.offlineReminder(req, tracked, started, err)
		}()
	}
	if t.scope != nil {
		if bindErr := t.scope.BindTask(tracked); bindErr != nil {
			return Result{}, bindErr
		}
	}
	conversation := c.store.Conversation(conversationID)
	saved := conversation.Sessions[selected.ID]
	saved.ConversationID = conversationID
	extras, agentToken, err := c.gateExtras(ctx, conversationID, selected, saved)
	if err != nil {
		return Result{}, err
	}
	saved.AgentToken = agentToken
	clock.mark("gate")
	extras = append(extras, c.projectMemory(ctx, conversationID, req)...)
	capabilities, err := c.assemble(selected, req, extras)
	if err != nil {
		return Result{}, err
	}
	clock.mark("assemble")
	if saved.Tainted {
		return Result{}, UserError{Text: c.text.T(i18n.Tainted, protocol.CommandNew)}
	}
	contextChanged := saved.HarnessID != "" && saved.CapabilityHash != capabilities.Fingerprint
	if contextChanged {
		// Only identity and platform guidance can change in place. A missing baseline cannot
		// prove that MCP connections, skills and visibility stayed the same.
		if saved.SessionConfigHash == "" || saved.SessionConfigHash != capabilities.SessionFingerprint {
			return Result{}, UserError{Text: c.text.T(i18n.CapabilityDrift, protocol.CommandNew)}
		}
		saved.InstructionsApplied = false
	}
	if saved.HarnessID != "" && sessionDrifted(saved, binding, workspace.Path) {
		return Result{}, UserError{Text: c.text.T(i18n.WorkspaceDrift, protocol.CommandNew)}
	}
	t.saved, t.capabilities, t.contextChanged = saved, capabilities, contextChanged
	// The turn is an attempt from here: leased on the project's canonical
	// workspace, renewed while it runs, and closed with whatever happened.
	spec, candidate, err := c.turnSpec(ctx, req, selected, tracked, binding, workspace)
	if err != nil {
		return Result{}, err
	}
	run, runErr := lifecycle.Run(ctx, t.options(spec, candidate))
	result, err = t.settle(parent, run, runErr)
	if run.Record.ID != "" {
		clock.report(parent, run.Record.ID)
	}
	return result, err
}

func (c *Coordinator) buildingProfile(req Request) (bool, error) {
	owner := injectionMode(req.ChatType, req.SenderOpenID, c.ownerOpenID) == home.ModeOwner
	if !onboard.Building(req.ConversationID, owner, true) {
		return false, nil
	}
	if editor, shared := c.home.(home.IdentityEditor); shared {
		return editor.NeedsInit()
	}
	return c.homePath != "" && home.NeedsInit(c.homePath), nil
}

func (c *Coordinator) open(ctx context.Context, saved state.Session, selected agent.Agent, workspace string, servers []acp.MCPServer) (harness.Runner, error) {
	if saved.HarnessID != "" && saved.HarnessID != selected.Harness {
		return nil, fmt.Errorf("session belongs to harness %q, not %q", saved.HarnessID, selected.Harness)
	}
	runner, err := c.runtime.OpenSession(ctx, placement(selected), saved.UpstreamID, workspace, servers)
	if err != nil {
		return nil, err
	}
	// A configured model preference applies to a fresh session only: a
	// resumed one keeps whatever the user last chose with /model. The agent
	// is the authority on what it offers, so a preference it cannot honour
	// is logged and skipped rather than failing the turn.
	if saved.UpstreamID == "" {
		// The owner's choices for this conversation sit over the agent's
		// configured defaults.
		if model, options := c.preferred(saved.ConversationID, selected); model != "" || len(options) > 0 {
			harness.ApplyPreferences(ctx, runner, selected.ID, model, options)
		}
	}
	return runner, nil
}

func (c *Coordinator) assemble(selected agent.Agent, req Request, extras []capability.Extra) (capability.Capabilities, error) {
	if c.home == nil {
		return c.assembler.AssembleExtra(selected, home.ModeNone, extras)
	}
	return c.assembler.AssembleExtra(selected, injectionMode(req.ChatType, req.SenderOpenID, c.ownerOpenID), extras)
}

// gateExtras injects the messaging server for harnesses that can speak HTTP
// MCP. The token is minted once per session and persisted with it, so the
// capability fingerprint stays stable across turns and gateway restarts.
func (c *Coordinator) gateExtras(ctx context.Context, conversationID string, selected agent.Agent, saved state.Session) ([]capability.Extra, string, error) {
	if c.gate == nil {
		return nil, saved.AgentToken, nil
	}
	supported, err := c.runtime.SupportsHTTPMCP(ctx, placement(selected))
	if err != nil {
		return nil, saved.AgentToken, err
	}
	if !supported {
		return nil, saved.AgentToken, nil
	}
	token := saved.AgentToken
	if token == "" {
		token, err = newAgentToken()
		if err != nil {
			return nil, "", err
		}
	}
	endpoint := ""
	if selected.Node != "" {
		if c.endpoints == nil {
			slog.Warn(fmt.Sprintf("turn: agent %q is on node %q with no endpoint resolver; messaging disabled", selected.ID, selected.Node), "conversation", conversationID, "agent", selected.ID, "node", selected.Node)
			return nil, saved.AgentToken, nil
		}
		endpoint, err = c.endpoints.MCPEndpoint(ctx, selected.Node)
		if err != nil {
			// Losing the send primitive costs milestone cards, not the
			// turn. The fingerprint changes, so the drift is visible
			// rather than a capability that quietly stopped working.
			slog.Warn(fmt.Sprintf("turn: node %q messaging endpoint: %v", selected.Node, err), "conversation", conversationID, "agent", selected.ID, "node", selected.Node)
			return nil, saved.AgentToken, nil
		}
	}
	return c.gate.Extras(conversationID, selected.ID, token, endpoint), token, nil
}

func newAgentToken() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("mint agent token: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

// turnSpend follows a turn's progress for what it cost and which model
// ran it, since the ACP prompt result carries neither.
type turnSpend struct {
	mu    sync.Mutex
	usage view.Usage
	used  string
	// touch resets the turn's idle clock: every report is a sign of life.
	touch func()
}

func (t *turnSpend) wrap(next func(view.Progress), agentID string) func(view.Progress) {
	return func(p view.Progress) {
		p.Agent = agentID
		if t.touch != nil {
			t.touch()
		}
		t.mu.Lock()
		t.usage = p.Usage
		if p.Settings.Model != "" {
			t.used = p.Settings.Model
		}
		t.mu.Unlock()
		if next != nil {
			next(p)
		}
	}
}

func (t *turnSpend) tokens() task.Tokens {
	t.mu.Lock()
	defer t.mu.Unlock()
	return task.FromUsage(t.usage.InputTokens, t.usage.OutputTokens, t.usage.CacheReadTokens, t.usage.CacheWriteTokens)
}

// attemptUsage is the spend in the ledger's shape; nil-safe.
func (t *turnSpend) attemptUsage() *attempt.Usage {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	u := t.usage
	return &attempt.Usage{
		Model: t.used, Input: int64(u.InputTokens), Output: int64(u.OutputTokens),
		CachedRead: int64(u.CacheReadTokens), CachedWrite: int64(u.CacheWriteTokens), Context: int64(u.ContextTokens),
		Reported: u.TokensReported(),
	}
}

func (t *turnSpend) model() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.used
}

func promptTurn(ctx context.Context, runner harness.Runner, prompt string, req Request) (string, []string, error) {
	out := lifecycle.Drive{Session: runner, Prompt: prompt, Media: req.Images, Turn: true, Ask: req.OnAsk, AskUser: req.OnAskUser, Observe: req.OnProgress}.Run(ctx)
	return out.Answer, out.Activity, out.Err
}
