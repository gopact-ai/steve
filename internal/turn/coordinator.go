package turn

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/idle"
	"github.com/gopact-ai/steve/internal/intent"
	"github.com/gopact-ai/steve/internal/lifecycle"
	"github.com/gopact-ai/steve/internal/memory"
	"github.com/gopact-ai/steve/internal/models"
	"github.com/gopact-ai/steve/internal/onboard"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/roster"
	"github.com/gopact-ai/steve/internal/schedule"
	"github.com/gopact-ai/steve/internal/skills"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/view"
)

type Request struct {
	Locale         string
	Channel        string
	ConversationID string
	Input          string
	SenderOpenID   string
	ChatType       protocol.ChatType
	Mentioned      bool
	// MessageID and ChatID anchor the turn in the channel, so an
	// interrupted task can be resumed and delivered after a restart.
	MessageID string
	ChatID    string
	// CardID is the platform's own card opened for this turn, journaled
	// so a crash can recall it instead of leaving a forever-running card.
	CardID     string
	Images     []harness.Media
	OnProgress func(view.Progress)
	OnAskUser  acphost.AskUserFunc
	// OnTurnReady binds human requests to the admitted task and attempt.
	OnTurnReady func(taskID, attemptID string)
	// Origin marks a prompt Steve sent on the user's behalf rather than one
	// they typed — a schedule firing, say. It rides onto the task so
	// unattended work stays recognisable after the fact.
	Origin string
	// Relocation is the original input and scoped history from its adapter.
	Relocation *RelocationContext
	// ExpectedProject fences an unattended submission to its creation-time project.
	ExpectedProject string
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
	OpenSession(context.Context, harness.Placement, string, string, []acp.MCPServer) (harness.Runner, error)
	CloseSession(context.Context, harness.Placement, string) error
	// SupportsHTTPMCP reports whether the harness's agent accepts HTTP MCP
	// servers, so the messaging capability is only injected where it works.
	SupportsHTTPMCP(context.Context, harness.Placement) (bool, error)
}

// AgentGate is the built-in messaging MCP server: given a session's identity
// and token it returns the capability to inject, binding the token to that
// conversation so the agent can never write anywhere else.
type AgentGate interface {
	Extras(conversationID, agentID, token, endpoint string) []capability.Extra
}

// NodeEndpoints resolves the messaging URL an agent on a given node must
// call. Only remote placements consult it.
type NodeEndpoints interface {
	MCPEndpoint(ctx context.Context, node string) (string, error)
}

// Injected is what a turn actually gave the agent, kept so "what did it
// see" can be answered from the record rather than recomputed from a
// configuration that may since have changed.
type Injected struct {
	Project, Workspace          string
	Agent, Node, Harness, Model string
	Options                     map[string]string
	Session                     string
	NewSession                  bool
	// InstructionsSent says the assembled instructions (identity, skills,
	// memory) went in front of this turn's prompt: at session start or
	// after an identity change. Instructions holds the text only when sent.
	InstructionsSent  bool
	Instructions      string
	InstructionsBytes int
	MCPServers        []string
	Fingerprint       string
	// Prompt is the text of this turn as sent, without the instructions.
	Prompt string
}

type Result struct {
	AgentID  string
	Title    string
	Text     string
	Activity []string
	Fields   []view.Field
	// Injected is what the agent was given for this turn.
	Injected *Injected
	// Attempt is the attempt the turn ran as; its record holds the
	// before and after snapshots.
	Attempt string
	// Recover marks a result whose card should offer to restore the
	// just-archived session — the /clear confirmation.
	Recover bool
}

type Coordinator struct {
	*coordinatorState
	text        i18n.Catalog
	ownerOpenID string
}

// coordinatorState owns shared execution state. Request-local views replace
// the owner and text catalog; mutexes and runtime state are never copied.
type coordinatorState struct {
	requestMu     sync.RWMutex
	maintaining   bool
	executions    *execution.Registry
	catalog       *agent.Catalog
	store         *state.Store
	assembler     *capability.Assembler
	runtime       runtime
	timeout       time.Duration
	channelOwners map[string]string
	home          home.Loader
	homePath      string
	skills        *skills.Live
	gate          AgentGate
	endpoints     NodeEndpoints
	RegisterIdle  idle.Registrar
	tasks         *task.Store
	// modes is how each conversation last reached Steve, for a tool call
	// that has no request to read it from.
	modes map[string]home.Mode
	// memory is what Steve remembers, by scope; nil until wired.
	memory      *memory.Service
	schedules   *schedule.Store
	supervisor  Supervisor
	plans       *plan.Store
	fleet       *roster.Roster
	refresher   Refresher
	files       MachineFiles
	probeOne    func(ctx context.Context, node, harness string) error
	probeAll    func(ctx context.Context) []models.Result
	projects    *project.Store
	attempts    *attempt.Service
	artifacts   *artifact.Store
	intents     *intent.Service
	disclosures map[string]held
	// defaultProject binds a fresh conversation; homeProject binds the
	// owner's DM, where Steve's own home directory is the project.
	defaultProject    string
	homeProject       string
	node              string
	resumer           func(TaskResume)
	notifier          func(TaskNotice)
	afterTurn         func(taskID string)
	planRecoveryOwner func(task.Task) bool
	// offlineAfter is how long a turn runs before its completion also earns
	// a plain-text ping; zero keeps Steve quiet.
	offlineAfter time.Duration

	mu            sync.Mutex
	lastSeen      map[string]time.Time
	active        map[string]harness.Runner
	cancels       map[string]*turnEntry
	cancelPending map[string]time.Time
	skillsLock    int
}

// turnEntry tracks one in-flight prompt turn. The blocked prompt goroutine
// owns session cleanup; done is closed by clearActive once it has finished,
// so /cancel can confirm the turn ended before deciding to force-kill.
type turnEntry struct {
	err    error
	cancel context.CancelFunc
	done   chan struct{}
}

func New(catalog *agent.Catalog, store *state.Store, assembler *capability.Assembler, runtime runtime, timeout time.Duration) *Coordinator {
	return &Coordinator{
		text: i18n.New(i18n.LocaleZH),
		coordinatorState: &coordinatorState{
			catalog: catalog, store: store, assembler: assembler, runtime: runtime, timeout: timeout,
			active: map[string]harness.Runner{}, cancels: map[string]*turnEntry{},
			cancelPending: map[string]time.Time{},
		},
	}
}

func (c *Coordinator) SetIdentity(ownerOpenID string, loader home.Loader) {
	c.ownerOpenID = ownerOpenID
	c.home = loader
	if dir, ok := loader.(home.Dir); ok {
		c.homePath = dir.Path
	}
}

// placement is where this agent's process belongs. It comes from the
// catalog rather than the saved session so a config change moves the agent
// on the next /new; a live session keeps its own node because its workspace
// and conversation are over there.
func placement(selected agent.Agent) harness.Placement {
	return harness.Placement{Node: selected.Node, Harness: selected.Harness}
}

// SetArtifacts wires the artifact store: chat turns get before- and
// after-snapshots, plans get a base and a landing.
func (c *Coordinator) SetArtifacts(store *artifact.Store) {
	c.artifacts = store
}

// SetAttempts wires the attempt service: every chat turn becomes an
// attempt, leased and fenced, from here on.
func (c *Coordinator) SetAttempts(service *attempt.Service) {
	c.attempts = service
}

// SetProjects wires the project store. defaultID binds a conversation that
// has never chosen; homeID, when set, binds the owner's DM instead.
func (c *Coordinator) SetProjects(store *project.Store, defaultID, homeID string) {
	c.projects = store
	c.defaultProject = defaultID
	c.homeProject = homeID
}

func (c *Coordinator) SetSkills(live *skills.Live) {
	c.skills = live
}

// SetAgentGate enables the send primitive: each session gets the messaging
// MCP server injected with its own conversation-bound token.
// SetNodeEndpoints wires the resolver remote placements need for messaging.
func (c *Coordinator) SetNodeEndpoints(e NodeEndpoints) { c.endpoints = e }

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
func (c *Coordinator) SetExecution(r *execution.Registry) { c.executions = r }

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
	c.requestMu.RLock()
	defer c.requestMu.RUnlock()
	var err error
	c, err = c.forChannel(req.Channel)
	if err != nil {
		return Result{}, err
	}
	c = c.localized(i18n.ContextLocale(ctx))
	if req.Locale != "" {
		c = c.localized(i18n.FromLang(req.Locale))
	}
	if c.maintaining {
		return Result{}, UserError{Text: c.text.T(i18n.HubMaintenance)}
	}
	ctx = i18n.WithLocale(ctx, c.text.Locale())
	return c.handle(ctx, req)
}

func (c *Coordinator) handle(ctx context.Context, req Request) (Result, error) {
	// Every arriving message is evidence that someone is present. The
	// offline reminder reads exactly this: nothing arrived while the turn
	// ran, so the person who asked is no longer watching.
	c.noteActivity(req.ConversationID)
	c.rememberMode(req)
	selected, prompt, switchOnly, err := c.selectAgent(req.ConversationID, req.Input)
	if err != nil {
		return Result{}, err
	}
	selected = c.recoveryAgent(req.ConversationID, selected, req.Origin)
	if switchOnly {
		return Result{AgentID: selected.ID, Text: c.text.T(i18n.Switched, selected.ID)}, nil
	}
	// Queueing is the default: a new message normally adds work after the
	// running turn rather than replacing it. A leading "!" is the explicit
	// interrupt — "stop that, do this instead" — so a correction is one
	// deliberate gesture instead of the accidental fate of every message.
	// "+" still queues for muscle memory from when interrupting was the
	// default; it is now a no-op alias.
	parsed := ParseInput(prompt)
	prompt, req.Queue = parsed.Prompt, !parsed.Interrupt
	cmd, rest := parsed.Command, parsed.Rest
	switch cmd {
	case protocol.CommandNew, protocol.CommandClear:
		return c.reset(ctx, req.ConversationID, selected)
	case protocol.CommandStatus:
		return c.status(req, selected), nil
	case protocol.CommandCancel:
		return c.cancel(ctx, req.ConversationID, selected)
	case protocol.CommandSkills:
		return c.skillsCmd(ctx, req, selected, rest)
	case protocol.CommandTasks:
		return c.tasksCmd(ctx, req, rest), nil
	case protocol.CommandEvery, protocol.CommandAt:
		return c.scheduleCmd(req, selected, cmd, rest), nil
	case protocol.CommandSchedules:
		return c.schedulesCmd(req, rest), nil
	case protocol.CommandModel:
		return c.modelCmd(ctx, req, selected, rest)
	case protocol.CommandHistory:
		return c.historyCmd(req, selected, rest)
	case protocol.CommandPlan:
		return c.planCmd(ctx, req, rest)
	case protocol.CommandPlans:
		return c.plansCmd(req, rest), nil
	case protocol.CommandFleet:
		if strings.TrimSpace(rest) == "probe" {
			return c.probeCmd(ctx), nil
		}
		return c.fleetCmd(ctx, req), nil
	case protocol.CommandRepair:
		return c.repairCmd(ctx, req, rest), nil
	case protocol.CommandProject:
		return c.projectCmd(ctx, req, rest)
	case protocol.CommandGrant:
		return c.grantCmd(ctx, req, rest)
	case protocol.CommandApprove, protocol.CommandDeny:
		return c.decideCmd(ctx, req, cmd, rest)
	case protocol.CommandEffects:
		return c.effectsCmd(ctx, req, rest)
	}
	result, err := c.prompt(ctx, req, selected, prompt)
	if err != nil {
		return result, err
	}
	// Content of a sealed project leaves only with the owner's approval.
	return c.gateDisclosure(ctx, req, result)
}

func (c *Coordinator) selectAgent(conversationID, input string) (agent.Agent, string, bool, error) {
	if c.catalog.Default().ID == "" {
		return agent.Agent{}, "", false, UserError{Text: c.text.T(i18n.NoAgentConfigured)}
	}
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
		log.Printf("turn: timing attempt=%s %s", run.Record.ID, clock)
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

func (c *Coordinator) reset(ctx context.Context, conversationID string, selected agent.Agent) (Result, error) {
	c.mu.Lock()
	busy := c.cancels[sessionKey(conversationID, selected.ID)] != nil
	c.mu.Unlock()
	if busy {
		return Result{}, UserError{Text: c.text.T(i18n.TurnBusy, protocol.CommandCancel)}
	}
	session := c.store.Conversation(conversationID).Sessions[selected.ID]
	if err := c.runtime.CloseSession(ctx, harness.Placement{Node: session.NodeID, Harness: session.HarnessID}, session.UpstreamID); err != nil {
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
			log.Printf("turn: agent %q is on node %q with no endpoint resolver; messaging disabled", selected.ID, selected.Node)
			return nil, saved.AgentToken, nil
		}
		endpoint, err = c.endpoints.MCPEndpoint(ctx, selected.Node)
		if err != nil {
			// Losing the send primitive costs milestone cards, not the
			// turn. The fingerprint changes, so the drift is visible
			// rather than a capability that quietly stopped working.
			log.Printf("turn: node %q messaging endpoint: %v", selected.Node, err)
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
			// and let its owner report whether the session really stopped.
			entry.cancel()
			return Result{}, err
		}
	} else {
		// The turn has begun but has not reached the agent yet
		// (assemble/open/save). Cancel the turn context immediately so
		// it cannot proceed into Prompt after /cancel.
		entry.cancel()
	}
	// Give the agent a chance to stop gracefully; the blocked prompt
	// goroutine owns session cleanup. A timeout reports uncertainty; it
	// cannot safely kill a shared host or claim the writer has stopped.
	select {
	case <-entry.done:
	case <-time.After(10 * time.Second):
		entry.cancel()

		select {
		case <-entry.done:
		case <-time.After(10 * time.Second):
			return Result{}, fmt.Errorf("%w: prompt has not acknowledged cancellation", harness.ErrStopUnconfirmed)
		}
	}
	if errors.Is(entry.err, harness.ErrStopUnconfirmed) {
		return Result{}, entry.err
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

func sessionKey(conversationID, agentID string) string { return conversationID + "\x00" + agentID }
