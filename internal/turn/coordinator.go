package turn

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/onboard"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/sessions"
	"github.com/gopact-ai/steve/internal/skills"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/view"
)

type Request struct {
	ConversationID string
	Input          string
	SenderOpenID   string
	ChatType       protocol.ChatType
	Mentioned      bool
	// MessageID and ChatID anchor the turn in the channel, so an
	// interrupted task can be resumed and delivered after a restart.
	MessageID  string
	ChatID     string
	Images     []harness.Media
	OnProgress func(view.Progress)
	OnAskUser  acphost.AskUserFunc
	// Queue makes this prompt wait for the running turn instead of
	// interrupting it: "also do this after" rather than "stop, do this".
	Queue   bool
	OnPhase func(view.Phase)
	OnAsk   permission.AskFunc
}

func (r Request) phase(p view.Phase) {
	if r.OnPhase != nil {
		r.OnPhase(p)
	}
}

// UserError is safe to show on Feishu. Gateway replies Text verbatim.
type UserError struct{ Text string }

func (e UserError) Error() string { return e.Text }

type runtime interface {
	OpenSession(context.Context, string, string, string, []acp.MCPServer) (harness.Runner, error)
	CloseSession(context.Context, string, string) error
	// SupportsHTTPMCP reports whether the harness's agent accepts HTTP MCP
	// servers, so the messaging capability is only injected where it works.
	SupportsHTTPMCP(context.Context, string) (bool, error)
}

// AgentGate is the built-in messaging MCP server: given a session's identity
// and token it returns the capability to inject, binding the token to that
// conversation so the agent can never write anywhere else.
type AgentGate interface {
	Extras(conversationID, agentID, token string) []capability.Extra
}

type Result struct {
	AgentID  string
	Title    string
	Text     string
	Activity []string
	Fields   []view.Field
	// Recover marks a result whose card should offer to restore the
	// just-archived session — the /clear confirmation.
	Recover bool
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
	gate        AgentGate
	tasks       *task.Store
	node        string
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

// SetAgentGate enables the send primitive: each session gets the messaging
// MCP server injected with its own conversation-bound token.
func (c *Coordinator) SetAgentGate(gate AgentGate) {
	c.gate = gate
}

// ReviveSession clears the taint a crash left on the member's session so a
// resume turn can run against it. The session is exactly as consistent as
// the agent's own disk state, which the agent reloads on session/load.
func (c *Coordinator) ReviveSession(conversationID, agentID string) error {
	return c.store.ClearTaint(conversationID, agentID)
}

// SetTasks enables task tracking. It is optional: with no store the
// coordinator behaves exactly as before, which keeps the turn path testable
// without a filesystem.
func (c *Coordinator) SetTasks(store *task.Store, node string) {
	c.tasks = store
	c.node = node
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
	// Queueing is the default: a new message normally adds work after the
	// running turn rather than replacing it. A leading "!" is the explicit
	// interrupt — "stop that, do this instead" — so a correction is one
	// deliberate gesture instead of the accidental fate of every message.
	// "+" still queues for muscle memory from when interrupting was the
	// default; it is now a no-op alias.
	prompt = strings.TrimSpace(prompt)
	req.Queue = true
	for _, bang := range []string{"!", "！"} {
		if rest, ok := strings.CutPrefix(prompt, bang); ok && strings.TrimSpace(rest) != "" {
			req.Queue = false
			prompt = strings.TrimSpace(rest)
			break
		}
	}
	if req.Queue {
		for _, plus := range []string{"+", "＋"} {
			if rest, ok := strings.CutPrefix(prompt, plus); ok && strings.TrimSpace(rest) != "" {
				prompt = strings.TrimSpace(rest)
				break
			}
		}
	}
	cmd, rest := protocol.ParseCommand(prompt)
	switch cmd {
	case protocol.CommandNew, protocol.CommandClear:
		return c.reset(ctx, req.ConversationID, selected)
	case protocol.CommandStatus:
		return c.status(req, selected), nil
	case protocol.CommandCancel:
		return c.cancel(ctx, req.ConversationID, selected)
	case protocol.CommandSkills:
		return c.skillsCmd(req, selected, rest)
	case protocol.CommandTasks:
		return c.tasksCmd(req), nil
	case protocol.CommandModel:
		return c.modelCmd(ctx, req, selected, rest)
	case protocol.CommandHistory:
		return c.historyCmd(req, selected, rest)
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
	// The deadline starts now, not at arrival: a queued prompt must not
	// burn its own running time standing behind the turn it waited for.
	ctx, expire := context.WithTimeout(turnCtx, c.timeout)
	defer expire()
	if c.consumePendingCancel(sessionKey(conversationID, selected.ID)) {
		return Result{}, context.Canceled
	}
	// The task opens only after the turn lock is held, so a rejected or
	// cancelled turn never spends a turn from the budget.
	tracked, taskErr := c.beginTask(req, selected, prompt)
	if taskErr != nil {
		return Result{}, taskErr
	}
	if tracked != "" {
		defer func() { c.finishTask(tracked, err) }()
	}
	conversation := c.store.Conversation(conversationID)
	saved := conversation.Sessions[selected.ID]
	extras, agentToken, err := c.gateExtras(ctx, conversationID, selected, saved)
	if err != nil {
		return Result{}, err
	}
	saved.AgentToken = agentToken
	capabilities, err := c.assemble(selected, req, extras)
	if err != nil {
		return Result{}, err
	}
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
	req.phase(view.PhaseWaking)
	runner, err := c.open(ctx, saved, selected, workspace, capabilities.MCPServers)
	if err != nil && saved.UpstreamID != "" {
		// The saved upstream session could not be reopened; drop it and
		// start a fresh session in this same turn instead of failing once
		// and waiting for the user to send again.
		if stateErr := c.store.DeleteSession(conversationID, selected.ID); stateErr != nil {
			log.Printf("turn: delete unreopenable session state: %v", stateErr)
		}
		saved.UpstreamID = ""
		saved.InstructionsApplied = false
		runner, err = c.open(ctx, saved, selected, workspace, capabilities.MCPServers)
	}
	if err != nil {
		return Result{}, err
	}
	req.phase(view.PhaseRunning)
	session := state.Session{
		ConversationID: conversationID, AgentID: selected.ID, HarnessID: selected.Harness,
		UpstreamID: runner.ID(), Workspace: workspace, CapabilityHash: capabilities.Fingerprint,
		InstructionsApplied: saved.InstructionsApplied, Tainted: true,
		AgentToken: saved.AgentToken,
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
	out, activity, err := promptTurn(ctx, runner, prompt, req)
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
	// Archive rather than delete. The agent session was closed, not deleted,
	// so the record is all that stands between the user and their own
	// history; dropping it would make a cleared conversation unreachable
	// forever.
	if err := c.store.ArchiveSession(conversationID, selected.ID, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return Result{}, err
	}
	c.closeTask(conversationID, selected.ID)
	return Result{AgentID: selected.ID, Text: c.text.T(i18n.Reset, selected.ID), Recover: true}, nil
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
	supported, err := c.runtime.SupportsHTTPMCP(ctx, selected.Harness)
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
	return c.gate.Extras(conversationID, selected.ID, token), token, nil
}

func newAgentToken() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("mint agent token: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

func (c *Coordinator) status(req Request, selected agent.Agent) Result {
	session := c.store.Conversation(req.ConversationID).Sessions[selected.ID]
	sid := session.UpstreamID
	if sid == "" {
		sid = "none"
	}
	title := c.text.T(i18n.CardStatus)
	fields := []view.Field{
		{Label: "Agent", Value: selected.ID, IsMetric: true},
	}
	fields = append(fields, c.taskFields(req.ConversationID, selected.ID)...)
	if c.home == nil {
		fields = append(fields,
			view.Field{Label: "Harness", Value: selected.Harness, IsMetric: true},
			view.Field{Label: "Session", Value: sid, Wide: true},
		)
		return statusResult(selected.ID, title, fields)
	}
	if injectionMode(req.ChatType, req.SenderOpenID, c.ownerOpenID) != home.ModeOwner {
		fields = append(fields,
			view.Field{Label: "Mode", Value: "guest", IsMetric: true},
			view.Field{Label: "Harness", Value: selected.Harness, Wide: true},
			view.Field{Label: "Session", Value: sid, Wide: true},
			view.Field{Label: "Home", Value: "guest", Wide: true},
		)
		return statusResult(selected.ID, title, fields)
	}
	snap, err := c.home.Load(home.ModeOwner)
	if err != nil {
		fields = append(fields,
			view.Field{Label: "Mode", Value: "error", IsMetric: true},
			view.Field{Label: "Home", Value: "error", Wide: true},
		)
		return statusResult(selected.ID, title, fields)
	}
	fields = append(fields,
		view.Field{Label: "Mode", Value: "owner", IsMetric: true},
		view.Field{Label: "Harness", Value: selected.Harness, Wide: true},
		view.Field{Label: "Session", Value: sid, Wide: true},
		view.Field{Label: "Home", Value: snap.Path, Wide: true},
		view.Field{
			Label: "Identity",
			Value: "Soul " + fileOK(snap.Soul) + " · User " + fileOK(snap.User) + " · Memory " + memorySize(snap.Memory),
			Wide:  true,
		},
	)
	if skills := c.skillStatusLine(); skills != "" {
		fields = append(fields, view.Field{Label: "Skills", Value: strings.TrimPrefix(skills, "skills="), Wide: true})
	}
	return statusResult(selected.ID, title, fields)
}

func statusResult(agentID, title string, fields []view.Field) Result {
	rows := make([]string, 0, len(fields))
	for _, field := range fields {
		rows = append(rows, "**"+field.Label+"**  "+field.Value)
	}
	return Result{AgentID: agentID, Title: title, Text: strings.Join(rows, "\n"), Fields: fields}
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

func promptTurn(ctx context.Context, runner harness.Runner, prompt string, req Request) (string, []string, error) {
	if turn, ok := runner.(harness.TurnRunner); ok {
		return turn.PromptTurn(ctx, prompt, req.Images, req.OnAsk, req.OnAskUser, req.OnProgress)
	}
	return runner.Prompt(ctx, prompt, req.OnProgress)
}

func sessionKey(conversationID, agentID string) string { return conversationID + "\x00" + agentID }
