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
	"sync"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/lifecycle"
	"github.com/gopact-ai/steve/internal/onboard"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/view"
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
	// while they are still answering. Preparation runs under it once;
	// started resets it when the prompt is sent.
	idleCtx, expire, touch := c.newIdleClock(turnCtx, selected.Node)
	var ctx context.Context = idleCtx
	defer expire()
	if c.consumePendingCancel(sessionKey(conversationID, selected.ID)) {
		return Result{}, context.Canceled
	}
	t := &chatTurn{c: c, req: req, selected: selected, clock: clock, prompt: prompt}
	binding, scope, scopeErr := c.beginTurnScope(ctx, req, selected.ID)
	if scopeErr != nil {
		return Result{}, scopeErr
	}
	if scope != nil {
		t.scope = scope
		defer func() { scope.Finish(t.unresolved(err)) }()
		ctx = scope.Context()
	}
	// The directory is settled before the task opens: a turn that has
	// nowhere to run has not started and spends nothing.
	workspace, err := c.workspaceFor(ctx, req, selected, binding)
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
	spent := &turnSpend{resetIdle: touch}
	req.OnProgress = spent.wrap(req.OnProgress, selected.ID)
	t.req, t.spent, t.tracked, t.binding, t.workspace = req, spent, tracked, binding, workspace
	var settled bool
	var settledErr error
	if tracked != "" {
		defer func() {
			finishErr := err
			if settled {
				finishErr = settledErr
			}
			if accountingErr := t.settleTask(parent, started, finishErr); accountingErr != nil {
				result = Result{}
				err = c.retainedBlocked("accounting", i18n.RetainedTriedCommitAccounting, i18n.RetainedProblemAccounting,
					accountingErr.Error(), i18n.RetainedAdviceRetryAccounting, errors.Join(err, accountingErr))
			}
		}()
	}
	if t.scope != nil {
		if bindErr := t.scope.BindTask(tracked); bindErr != nil {
			return Result{}, bindErr
		}
	}
	req.stage(view.StageCapabilities)
	if err := t.prepareSession(ctx); err != nil {
		return Result{}, err
	}
	// The turn is an attempt from here: leased on the project's canonical
	// workspace, renewed while it runs, and closed with whatever happened.
	spec, candidate, err := c.turnSpec(ctx, req, selected, tracked, binding, workspace)
	if err != nil {
		return Result{}, err
	}
	req.stage(view.StagePlacement)
	run, runErr := lifecycle.Run(ctx, t.options(spec, candidate))
	result, err = t.settle(parent, run, runErr)
	// Disclosure must remain inside the active turn, but its persistence
	// error must not rewrite the already-settled agent execution outcome.
	settled, settledErr = true, err
	if run.Record.ID != "" {
		clock.report(parent, run.Record.ID)
	}
	if err == nil {
		return c.gateDisclosure(parent, req, result)
	}
	return result, err
}

// settleTask charges the task a tracked turn opened with what the turn
// spent and ended with, then tells whoever is waiting on it. It returns
// the accounting error that has to replace the turn's outcome — the
// result is kept, the charge is not committed yet. A managed stop the
// harness could not confirm is left open for the stop to settle.
func (t *chatTurn) settleTask(parent context.Context, started time.Time, finishErr error) error {
	c, req, tracked := t.c, t.req, t.tracked
	if t.managed && errors.Is(finishErr, harness.ErrStopUnconfirmed) {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 15*time.Second)
	defer cancel()
	if err := c.finishChatAccounting(ctx, tracked, t.run.Record, finishErr, t.spent.tokens(), t.spent.model()); err != nil {
		return err
	}
	var step *lifecycle.StepError
	errors.As(finishErr, &step)
	if t.run.Record.ID != "" && (step == nil || step.Step >= lifecycle.StepArm) {
		c.notifyAccountedTurn(tracked)
	}
	if onboarding(req) {
		// The introduction is delivered by onboarding itself, not as
		// a task notice into a chat that does not exist yet.
		c.closeOnboardingTask(req, tracked, finishErr)
		return nil
	}
	c.offlineReminder(req, tracked, started, finishErr)
	return nil
}

func (t *chatTurn) prepareSession(ctx context.Context) error {
	c, req, selected := t.c, t.req, t.selected
	conversation := c.store.Conversation(req.ConversationID)
	saved := conversation.Sessions[selected.ID]
	saved.ConversationID = req.ConversationID
	if saved.HarnessID != "" && saved.HarnessID != selected.Harness {
		return fmt.Errorf("session belongs to harness %q, not %q", saved.HarnessID, selected.Harness)
	}
	extras, agentToken, endpoint, err := c.describeGateExtras(ctx, selected, saved)
	if err != nil {
		return err
	}
	saved.AgentToken = agentToken
	gateEnabled := len(extras) > 0
	t.clock.mark("gate")
	extras = append(extras, c.projectMemory(ctx, req.ConversationID, req)...)
	var capabilities capability.Capabilities
	if saved.PluginRuntime != nil {
		mode := injectionMode(req.ChatType, req.SenderOpenID, c.ownerOpenID)
		capabilities, err = c.assembler.AssembleExtraPinned(selected, mode, extras, saved.PluginSkillsFingerprint)
	} else {
		capabilities, err = c.assemble(selected, req, extras)
	}
	if err != nil {
		return err
	}
	t.clock.mark("assemble")
	if saved.Tainted {
		cleared, taintErr := c.clearSettledTaint(ctx, req.ConversationID, selected.ID, saved)
		if taintErr != nil {
			return taintErr
		}
		if !cleared {
			return UserError{Text: c.text.T(i18n.Tainted, protocol.CommandNew)}
		}
		saved.Tainted = false
	}
	contextChanged := saved.HarnessID != "" && (saved.NativeImport == nil || saved.UpstreamID != "") && saved.CapabilityHash != capabilities.Fingerprint
	if contextChanged {
		// Only identity and platform guidance can change in place. A missing baseline cannot
		// prove that MCP connections, skills and visibility stayed the same.
		if saved.SessionConfigHash == "" || saved.SessionConfigHash != capabilities.SessionFingerprint {
			return UserError{Text: c.text.T(i18n.CapabilityDrift, protocol.CommandNew)}
		}
		saved.InstructionsApplied = false
	}
	if saved.HarnessID != "" && sessionDrifted(saved, t.binding, t.workspace.Path) {
		return UserError{Text: c.text.T(i18n.WorkspaceDrift, protocol.CommandNew)}
	}
	// Check the complete previous configuration before changing any credential.
	// An authorization rejection is not permission to change tools or workspace.
	if gateEnabled {
		if saved, capabilities, err = t.prepareCredentials(ctx, saved, capabilities, extras, endpoint); err != nil {
			return err
		}
	}
	t.saved, t.capabilities, t.contextChanged = saved, capabilities, contextChanged
	return nil
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
	ctx = harness.WithPluginProfile(ctx, saved.PluginRuntime)
	ctx = harness.WithNativeImport(ctx, saved.NativeImport)
	if saved.HarnessID != "" && saved.HarnessID != selected.Harness {
		return nil, fmt.Errorf("session belongs to harness %q, not %q", saved.HarnessID, selected.Harness)
	}
	runner, err := c.runtime.OpenSession(ctx, placement(selected), saved.UpstreamID, workspace, servers)
	if err != nil {
		return nil, err
	}
	// Resume the native conversation first, then apply its current preferences.
	// Defaults do not override an existing model, but an explicit conversation
	// choice (including /model) must survive process replacement.
	if saved.NativeImport != nil {
		return runner, nil
	}
	unlock := c.lockPreferences(saved.ConversationID, selected.ID)
	defer unlock()
	model, options := c.preferred(saved.ConversationID, selected)
	if saved.UpstreamID != "" {
		model = c.store.Preferences(saved.ConversationID, selected.ID)["model"]
	}
	harness.ApplyPreferences(ctx, runner, selected.ID, model, options, selected.Approval)
	return runner, nil
}

func (c *Coordinator) assemble(selected agent.Agent, req Request, extras []capability.Extra) (capability.Capabilities, error) {
	return c.assembler.AssembleExtra(selected, injectionMode(req.ChatType, req.SenderOpenID, c.ownerOpenID), extras)
}

// gateExtras injects the messaging server for harnesses that can speak HTTP
// MCP. The token is minted once per session and persisted with it, so the
// capability fingerprint stays stable across turns and gateway restarts.
func (c *Coordinator) gateExtras(ctx context.Context, conversationID string, selected agent.Agent, saved state.Session) ([]capability.Extra, string, error) {
	extras, token, endpoint, err := c.describeGateExtras(ctx, selected, saved)
	if err != nil || len(extras) == 0 {
		return extras, token, err
	}
	extras, err = c.gate.PrepareExtras(conversationID, selected.ID, token, endpoint)
	return extras, token, err
}

// describeGateExtras is side-effect free: drift must be checked before any
// credential is prepared or an existing binding is revoked.
func (c *Coordinator) describeGateExtras(ctx context.Context, selected agent.Agent, saved state.Session) ([]capability.Extra, string, string, error) {
	if c.gate == nil {
		return nil, saved.AgentToken, "", nil
	}
	supported, err := c.runtime.SupportsHTTPMCP(ctx, placement(selected))
	if err != nil {
		return nil, saved.AgentToken, "", err
	}
	if !supported {
		return nil, saved.AgentToken, "", nil
	}
	token := saved.AgentToken
	if token == "" {
		token = newAgentToken()
	}
	endpoint := ""
	if selected.Node != "" {
		if c.nodes == nil {
			return nil, saved.AgentToken, "", fmt.Errorf("turn: no MCP endpoint resolver for node %q", selected.Node)
		}
		endpoint, err = c.nodes.MCPEndpoint(ctx, selected.Node)
		if err != nil {
			return nil, saved.AgentToken, "", fmt.Errorf("turn: resolve node %q MCP endpoint: %w", selected.Node, err)
		}
	}
	return c.gate.DescribeExtras(token, endpoint), token, endpoint, nil
}

// clearSettledTaint lets a conversation carry on after an interrupted turn.
// The taint only records that the previous turn never reported how it ended.
// Once no attempt on that session is still live or awaiting confirmation,
// nothing can still be writing to it, and reopening it is the same accepted
// risk the recovery path takes: the agent replays its own history on load.
func (c *Coordinator) clearSettledTaint(ctx context.Context, conversationID, agentID string, saved state.Session) (bool, error) {
	if saved.UpstreamID == "" {
		return false, nil
	}
	live, err := c.attempts.Live(ctx)
	if err != nil {
		return false, err
	}
	for _, r := range live {
		if r.Session == saved.UpstreamID || r.NativeContext == saved.UpstreamID {
			return false, nil
		}
	}
	if err := c.store.ClearTaint(conversationID, agentID); err != nil {
		return false, err
	}
	return true, nil
}

func newAgentToken() string {
	var raw [16]byte
	rand.Read(raw[:])
	return hex.EncodeToString(raw[:])
}

// turnSpend follows a turn's progress for what it cost and which model
// ran it, since the ACP prompt result carries neither.
type turnSpend struct {
	mu    sync.Mutex
	usage view.Usage
	used  string
	// resetIdle resets the turn's idle clock; nil when the turn has none.
	resetIdle func()
}

// touch resets the turn's idle clock, if it has one: every report is a
// sign of life.
func (t *turnSpend) touch() {
	if t.resetIdle != nil {
		t.resetIdle()
	}
}

func (t *turnSpend) wrap(next func(view.Progress), agentID string) func(view.Progress) {
	return func(p view.Progress) {
		p.Agent = agentID
		t.touch()
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
