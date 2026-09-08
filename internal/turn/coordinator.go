package turn

import (
	"context"
	"fmt"
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
	"github.com/gopact-ai/steve/internal/memory"
	"github.com/gopact-ai/steve/internal/models"
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
	if result, handled, err := c.commands().dispatch(ctx, req, selected, cmd, rest); handled {
		return result, err
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
