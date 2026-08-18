package turn

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/onboard"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/sessions"
	"github.com/gopact-ai/steve/internal/skills"
	"github.com/gopact-ai/steve/internal/state"
)

type Request struct {
	ConversationID string
	Input          string
	SenderOpenID   string
	ChatType       protocol.ChatType
	Mentioned      bool
}

// UserError is safe to show on Feishu. Gateway replies Text verbatim.
type UserError struct{ Text string }

func (e UserError) Error() string { return e.Text }

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
	catalog     *agent.Catalog
	store       *state.Store
	assembler   *capability.Assembler
	runtime     runtime
	timeout     time.Duration
	ownerOpenID string
	home        home.Loader
	homePath    string
	scanHome    string
	skills      *skills.Live
	text        i18n.Catalog

	mu            sync.Mutex
	active        map[string]harness.Runner
	cancels       map[string]*turnEntry
	cancelPending map[string]time.Time
	skillsLock    int
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
		text:   i18n.New(i18n.LocaleZH),
		active: map[string]harness.Runner{}, cancels: map[string]*turnEntry{},
		cancelPending: map[string]time.Time{},
	}
}

func (c *Coordinator) SetIdentity(ownerOpenID string, loader home.Loader) {
	c.ownerOpenID = ownerOpenID
	c.home = loader
	if dir, ok := loader.(home.Dir); ok {
		c.homePath = dir.Path
	}
}

func (c *Coordinator) sessionWorkspace(req Request, selected agent.Agent, saved state.Session) string {
	if saved.Workspace != "" {
		return saved.Workspace
	}
	if c.homePath != "" && injectionMode(req.ChatType, req.SenderOpenID, c.ownerOpenID) == home.ModeOwner {
		return c.homePath
	}
	return selected.Workspace
}

func (c *Coordinator) SetSkills(live *skills.Live) {
	c.skills = live
}

func (c *Coordinator) SetCatalog(cat i18n.Catalog) {
	c.text = cat
}

func injectionMode(chatType protocol.ChatType, sender, owner string) home.Mode {
	if owner != "" && sender == owner && chatType == protocol.ChatP2P {
		return home.ModeOwner
	}
	return home.ModeGuest
}

func (c *Coordinator) listenPrefix(req Request) string {
	if req.ChatType != protocol.ChatGroup || req.Mentioned {
		return ""
	}
	locale := home.LocaleZH
	if c.text.Locale() == i18n.LocaleEN {
		locale = home.LocaleEN
	}
	return home.ListenUnmentioned(locale) + "\n\n"
}

func (c *Coordinator) Handle(ctx context.Context, req Request) (Result, error) {
	selected, prompt, switchOnly, err := c.selectAgent(req.ConversationID, req.Input)
	if err != nil {
		return Result{}, err
	}
	if switchOnly {
		return Result{AgentID: selected.ID, Text: c.text.T(i18n.Switched, selected.ID)}, nil
	}
	cmd, rest := protocol.ParseCommand(strings.TrimSpace(prompt))
	switch cmd {
	case protocol.CommandNew, protocol.CommandClear:
		return c.reset(ctx, req.ConversationID, selected)
	case protocol.CommandStatus:
		return c.status(req, selected), nil
	case protocol.CommandCancel:
		return c.cancel(ctx, req.ConversationID, selected)
	case protocol.CommandSkills:
		return c.skillsCmd(req, selected, rest)
	}
	return c.prompt(ctx, req, selected, prompt)
}

func (c *Coordinator) selectAgent(conversationID, input string) (agent.Agent, string, bool, error) {
	if selection, ok := c.catalog.Select(input); ok {
		if err := c.store.SetActiveAgent(conversationID, selection.Agent.ID); err != nil {
			return agent.Agent{}, "", false, err
		}
		return selection.Agent, selection.Prompt, selection.SwitchOnly, nil
	}
	fields := strings.Fields(strings.TrimSpace(input))
	if len(fields) > 0 && (strings.HasPrefix(fields[0], "@") || fields[0] == string(protocol.CommandUse)) {
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

func (c *Coordinator) prompt(parent context.Context, req Request, selected agent.Agent, prompt string) (Result, error) {
	conversationID := req.ConversationID
	ctx, cancel := context.WithTimeout(parent, c.timeout)
	if !c.beginTurn(conversationID, selected.ID, cancel) {
		cancel()
		if c.skillsUpdating() {
			return Result{}, UserError{Text: c.text.T(i18n.SkillsUpdating)}
		}
		return Result{}, UserError{Text: c.text.T(i18n.TurnBusy, protocol.CommandCancel)}
	}
	defer c.clearActive(conversationID, selected.ID)
	if c.consumePendingCancel(sessionKey(conversationID, selected.ID)) {
		return Result{}, context.Canceled
	}
	capabilities, err := c.assemble(selected, req)
	if err != nil {
		return Result{}, err
	}
	conversation := c.store.Conversation(conversationID)
	saved := conversation.Sessions[selected.ID]
	if saved.Tainted {
		return Result{}, UserError{Text: c.text.T(i18n.Tainted, protocol.CommandNew)}
	}
	if saved.HarnessID != "" && saved.CapabilityHash != capabilities.Fingerprint {
		return Result{}, UserError{Text: c.text.T(i18n.CapabilityDrift, protocol.CommandNew)}
	}
	workspace := c.sessionWorkspace(req, selected, saved)
	if saved.HarnessID != "" && saved.Workspace != "" && saved.Workspace != workspace {
		return Result{}, UserError{Text: c.text.T(i18n.WorkspaceDrift, protocol.CommandNew)}
	}
	runner, err := c.open(ctx, saved, selected, workspace, capabilities.MCPServers)
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
		UpstreamID: runner.ID(), Workspace: workspace, CapabilityHash: capabilities.Fingerprint,
		InstructionsApplied: saved.InstructionsApplied, Tainted: true,
	}
	if err := c.store.SaveSession(session); err != nil {
		c.discard(parent, selected, runner)
		return Result{}, err
	}
	user := prompt
	if req.SenderOpenID != "" || req.ChatType != "" {
		speaker := req.SenderOpenID
		if speaker == "" {
			speaker = "-"
		}
		ownerFlag := "false"
		if req.SenderOpenID != "" && req.SenderOpenID == c.ownerOpenID {
			ownerFlag = "true"
		}
		user = fmt.Sprintf("[steve: speaker=%s owner=%s chat=%s]\n%s", speaker, ownerFlag, req.ChatType, prompt)
	}
	if prefix := c.listenPrefix(req); prefix != "" {
		user = prefix + user
	}
	building := c.buildingProfile(req)
	if building {
		user = onboard.Continue(c.text.Locale(), c.homePath, c.excerpts(prompt)) + "\n\n" + user
	}
	if !session.InstructionsApplied && capabilities.Instructions != "" {
		user = capabilities.Instructions + "\n\n" + user
	}
	prompt = user
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
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			// Agent error or cooperative cancel: drop the session and keep
			// the shared process. Only a deadline means the process may be
			// stuck and needs to be killed.
			if stateErr := c.store.DeleteSession(conversationID, selected.ID); stateErr != nil {
				log.Printf("turn: delete failed session state: %v", stateErr)
			}
			return Result{}, err
		}
		// The turn timed out while possibly still running.
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
		out += "\n\n" + c.text.T(i18n.StateSaveFailed, protocol.CommandNew)
	}
	if building {
		reply, _, applyErr := onboard.Apply(c.homePath, out)
		if applyErr != nil {
			log.Printf("turn: apply identity files: %v", applyErr)
		} else {
			out = reply
		}
		activity = nil
	}
	return Result{AgentID: selected.ID, Text: out, Activity: activity}, nil
}

func (c *Coordinator) buildingProfile(req Request) bool {
	owner := injectionMode(req.ChatType, req.SenderOpenID, c.ownerOpenID) == home.ModeOwner
	return onboard.Building(req.ConversationID, owner, c.homePath != "" && home.NeedsInit(c.homePath))
}

func (c *Coordinator) excerpts(input string) string {
	if !onboard.AllowScan(input) {
		return ""
	}
	root := c.scanHome
	if root == "" {
		var err error
		root, err = os.UserHomeDir()
		if err != nil {
			return ""
		}
	}
	return sessions.Format(sessions.Collect(root))
}

func (c *Coordinator) discard(parent context.Context, selected agent.Agent, runner harness.Runner) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 15*time.Second)
	defer cancel()
	if err := c.runtime.CloseSession(ctx, selected.Harness, runner.ID()); err != nil {
		runner.Abort()
	}
}

func (c *Coordinator) open(ctx context.Context, saved state.Session, selected agent.Agent, workspace string, servers []acp.MCPServer) (harness.Runner, error) {
	if saved.HarnessID != "" && saved.HarnessID != selected.Harness {
		return nil, fmt.Errorf("session belongs to harness %q, not %q", saved.HarnessID, selected.Harness)
	}
	return c.runtime.OpenSession(ctx, selected.Harness, saved.UpstreamID, workspace, servers)
}

func (c *Coordinator) reset(ctx context.Context, conversationID string, selected agent.Agent) (Result, error) {
	c.mu.Lock()
	busy := c.cancels[sessionKey(conversationID, selected.ID)] != nil
	c.mu.Unlock()
	if busy {
		return Result{}, UserError{Text: c.text.T(i18n.TurnBusy, protocol.CommandCancel)}
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
		return Result{}, UserError{Text: c.text.T(i18n.TurnBusy, protocol.CommandCancel)}
	}
	if err := c.store.DeleteSession(conversationID, selected.ID); err != nil {
		return Result{}, err
	}
	return Result{AgentID: selected.ID, Text: c.text.T(i18n.Reset, selected.ID)}, nil
}

func (c *Coordinator) assemble(selected agent.Agent, req Request) (capability.Capabilities, error) {
	if c.home == nil {
		return c.assembler.Assemble(selected)
	}
	return c.assembler.AssembleMode(selected, injectionMode(req.ChatType, req.SenderOpenID, c.ownerOpenID))
}

func (c *Coordinator) status(req Request, selected agent.Agent) Result {
	session := c.store.Conversation(req.ConversationID).Sessions[selected.ID]
	line := fmt.Sprintf("active=%s harness=%s session=%s", selected.ID, selected.Harness, session.UpstreamID)
	if session.UpstreamID == "" {
		line = fmt.Sprintf("active=%s harness=%s session=none", selected.ID, selected.Harness)
	}
	if c.home == nil {
		return Result{AgentID: selected.ID, Text: line}
	}
	if injectionMode(req.ChatType, req.SenderOpenID, c.ownerOpenID) != home.ModeOwner {
		return Result{AgentID: selected.ID, Text: line + "\nhome=guest"}
	}
	snap, err := c.home.Load(home.ModeOwner)
	if err != nil {
		return Result{AgentID: selected.ID, Text: line + "\nhome=error"}
	}
	homeLine := fmt.Sprintf("home=%s mode=owner soul=%s user=%s memory=%s",
		snap.Path, fileOK(snap.Soul), fileOK(snap.User), memorySize(snap.Memory))
	if skillLine := c.skillStatusLine(); skillLine != "" {
		homeLine += "\n" + skillLine
	}
	return Result{AgentID: selected.ID, Text: line + "\n" + homeLine}
}

func fileOK(body string) string {
	if strings.TrimSpace(body) == "" {
		return "missing"
	}
	return "ok"
}

func memorySize(body string) string {
	n := len([]byte(body))
	if n >= 1024 {
		return fmt.Sprintf("%dkiB", (n+1023)/1024)
	}
	return fmt.Sprintf("%dB", n)
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
			// and force-kill the harness process so the next turn starts
			// fresh instead of waiting out the prompt timeout.
			entry.cancel()
			runner.Abort()
			return Result{}, err
		}
	} else {
		// The turn has begun but has not reached the agent yet
		// (assemble/open/save). Cancel the turn context immediately so
		// it cannot proceed into Prompt after /cancel.
		entry.cancel()
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
	return Result{AgentID: selected.ID, Text: c.text.T(i18n.CancelRequested, selected.ID)}, nil
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
