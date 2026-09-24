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
	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/idle"
	"github.com/gopact-ai/steve/internal/intent"
	"github.com/gopact-ai/steve/internal/memory"
	"github.com/gopact-ai/steve/internal/models"
	"github.com/gopact-ai/steve/internal/nodewire"
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
	// ExchangeID is set by the console adapter for the exact durable input
	// being handled. Other channel message identities are not queue authority.
	ExchangeID string
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
	// ExpectedTask binds an automatic continuation to its original task.
	ExpectedTask    string
	ResumeAdmission task.ResumeAdmission
	// Queue makes this prompt wait for the running turn instead of
	// interrupting it: "also do this after" rather than "stop, do this".
	Queue   bool
	OnPhase func(view.Phase)
	// OnStage reports which preparation step is running while the turn is
	// still waking, so a long start says what it is waiting on.
	OnStage func(view.Stage)
	OnAsk   permission.AskFunc
}

func (r Request) phase(p view.Phase) {
	if r.OnPhase != nil {
		r.OnPhase(p)
	}
}

func (r Request) stage(s view.Stage) {
	if r.OnStage != nil {
		r.OnStage(s)
	}
}

// UserError is safe to show on Feishu. Gateway replies Text verbatim.
type UserError struct{ Text string }

func (e UserError) Error() string { return e.Text }

// Runtime opens and closes the agent sessions turns run in.
type Runtime interface {
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
	PrepareExtras(conversationID, agentID, token, endpoint string) ([]capability.Extra, error)
	DescribeExtras(token, endpoint string) []capability.Extra
}

// Nodes is what a coordinator asks of the machines agents run on.
type Nodes interface {
	// MCPEndpoint resolves the messaging URL an agent on node must call.
	// Only remote placements consult it.
	MCPEndpoint(ctx context.Context, node string) (string, error)
	// RegisterIdle holds a prompt's silence clock while node is
	// disconnected, until unregister is called.
	RegisterIdle(node string, clock idle.Clock) (unregister func())
	// Refresh asks node to check itself again, so its advert reflects a
	// repair that just ran.
	Refresh(ctx context.Context, node string) (nodewire.Advert, error)
	// Files reads platform file facts on node; "" is the hub.
	Files(ctx context.Context, node string, req nodewire.FileRequest) (string, error)
}

// ModelProber asks harnesses which models they run.
type ModelProber interface {
	// Probe asks one harness on node, the way a repair does for a harness
	// that just appeared.
	Probe(ctx context.Context, node, harness string) error
	// ProbeAll asks every harness on every machine, known or not.
	ProbeAll(ctx context.Context) []models.Result
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
	// Sections is what the instructions sent this turn were made of.
	Sections []capability.Section
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
	requestMu   sync.RWMutex
	maintaining bool
	executions  *execution.Registry
	catalog     *agent.Catalog
	store       *state.Store
	assembler   *capability.Assembler
	runtime     Runtime
	promptClock promptClock
	// autoResolveSource is read only at operation boundaries; a saved
	// setting never interrupts a live turn. Nil never hands a merge
	// conflict to an agent unasked.
	autoResolveSource func() bool
	channelOwners     map[string]string
	home              home.Loader
	homePath          string
	skills            *skills.Live
	// gate is Callbacks.AgentGate: nil when its port cannot be bound.
	gate  AgentGate
	nodes Nodes
	tasks *task.Store

	consoleCompletionGuard ConsoleCompletionGuard

	// modes is how each conversation last reached Steve, for a tool call
	// that has no request to read it from.
	modes map[string]home.Mode
	// memory is what Steve remembers, by scope.
	memory     *memory.Service
	schedules  *schedule.Store
	supervisor Supervisor
	plans      *plan.Store
	fleet      *roster.Roster
	prober     ModelProber
	projects   *project.Store
	// attach gives a project a directory on the machine an agent runs on
	// when it has none there.
	attach    func(ctx context.Context, projectID, node string) error
	attempts  *attempt.Service
	artifacts *artifact.Store
	// resolving guards a merge-conflict resolution in flight, which runs
	// far longer than the sweep interval that may ask for it again.
	resolving   map[string]bool
	intents     *intent.Service
	disclosures heldDisclosures
	// defaultProject binds a fresh conversation; homeProject binds the
	// owner's DM, where Steve's own home directory is the project.
	defaultProject string
	homeProject    string
	node           string
	// planRecoveryOwner is Deps.PlanRecoveryOwner; nil leaves every
	// retained plan to ResumePlans.
	planRecoveryOwner func(task.Task) bool
	// supervisor, attach, gate and the callbacks below are set once by
	// Wire. gate, afterTurn and turnPreface may be nil and are checked
	// where they are used; the rest are never nil once wired.
	resumer          func(TaskResume) error
	resumeDispatcher func(TaskResume)
	notifier         func(TaskNotice)
	afterTurn        func(taskID string)
	turnPreface      func(ctx context.Context, taskID string) Preface
	// offlineAfter is how long a turn runs before its completion also earns
	// a plain-text ping; zero keeps Steve quiet.
	offlineAfter time.Duration

	mu              sync.Mutex
	preferenceLocks sync.Map // conversation/agent -> *sync.Mutex
	lastSeen        map[string]time.Time
	active          map[string]harness.Runner
	cancels         map[string]*turnEntry
	cancelPending   map[string]time.Time
	skillsLock      int
}

// Deps is everything a Coordinator is built with. New refuses a Deps
// missing anything Deps.required lists; every other field may be zero.
type Deps struct {
	Catalog   *agent.Catalog
	Store     *state.Store
	Assembler *capability.Assembler
	Runtime   Runtime
	// Timeout is how long a turn may go without progress before it is
	// cancelled, and the deadline of a /model command and of reading an
	// agent's selectors. TimeoutSource, when set, is read in its place
	// each time one of them starts.
	Timeout       time.Duration
	TimeoutSource func() time.Duration
	// AutoResolveSource decides at each sweep whether a merge conflict is
	// handed to an agent without anyone asking; nil never does.
	AutoResolveSource func() bool
	Text              i18n.Catalog
	// Owner is the baseline owner identity; ChannelOwners registers each
	// trusted non-console adapter's native owner.
	Owner         string
	ChannelOwners map[string]string
	Home          home.Loader
	Skills        *skills.Live
	Projects      *project.Store
	// DefaultProject binds a conversation that has never chosen;
	// HomeProject, when set, binds the owner's DM instead.
	DefaultProject string
	HomeProject    string
	Memory         *memory.Service
	Attempts       *attempt.Service
	Artifacts      *artifact.Store
	Intents        *intent.Service
	Executions     *execution.Registry
	Tasks          *task.Store
	// Node is the name tasks record as the machine that tracks them.
	Node      string
	Schedules *schedule.Store
	// OfflineAfter is how long a turn runs before its completion also
	// earns a plain-text ping; zero keeps Steve quiet.
	OfflineAfter time.Duration
	// ConsoleCompletionGuard checks the console's facts inside the
	// transaction closing a task tree; nil refuses to close one while any
	// console fact exists.
	ConsoleCompletionGuard ConsoleCompletionGuard
	// Nodes resolves remote messaging endpoints, holds a prompt's silence
	// clock while its node is disconnected, and refreshes and reads the
	// machines a repair works on. Without it a remote agent's turn is
	// refused while messaging is on, a disconnection counts as silence, a
	// repair leaves the machine's advert as it was, and the PATH a repair
	// reports is unknown.
	Nodes Nodes
	// PlanRecoveryOwner reports a task whose transport resumes its own
	// retained plan, preserving the original exchange, progress and asks;
	// ResumePlans leaves such a task alone. Nil leaves none alone.
	PlanRecoveryOwner func(task.Task) bool
	// Plans holds every plan the planning verbs draft and run.
	Plans *plan.Store
	// Fleet admits and places agents on the machines they run on; without
	// it no admission runs and /fleet reports a hub alone.
	Fleet *roster.Roster
	// Prober answers `/fleet probe` and fills a repaired harness's model.
	Prober ModelProber
}

// dependency is one Deps field New refuses to build without.
type dependency struct {
	name   string
	absent bool
}

// required is every Deps field New refuses to build without. Each is named
// after the Coordinator field it fills, lower-cased: a field on this list is
// never nil once New returns.
func (d Deps) required() []dependency {
	return []dependency{
		{"Catalog", d.Catalog == nil}, {"Store", d.Store == nil}, {"Assembler", d.Assembler == nil},
		{"Runtime", d.Runtime == nil}, {"Text", d.Text.IsZero()}, {"Home", d.Home == nil},
		{"Skills", d.Skills == nil}, {"Projects", d.Projects == nil}, {"Memory", d.Memory == nil},
		{"Attempts", d.Attempts == nil}, {"Artifacts", d.Artifacts == nil}, {"Intents", d.Intents == nil},
		{"Executions", d.Executions == nil}, {"Tasks", d.Tasks == nil}, {"Schedules", d.Schedules == nil},
		{"Plans", d.Plans == nil}, {"Prober", d.Prober == nil},
	}
}

// New builds a Coordinator from deps, or reports every dependency missing.
func New(deps Deps) (*Coordinator, error) {
	var missing []string
	for _, dep := range deps.required() {
		if dep.absent {
			missing = append(missing, dep.name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("turn: missing dependencies: %s", strings.Join(missing, ", "))
	}
	owners, err := channelOwners(deps.ChannelOwners)
	if err != nil {
		return nil, fmt.Errorf("turn: %w", err)
	}
	homePath := ""
	if dir, ok := deps.Home.(home.Dir); ok {
		homePath = dir.Path
	}
	return &Coordinator{
		text:        deps.Text,
		ownerOpenID: deps.Owner,
		coordinatorState: &coordinatorState{
			catalog: deps.Catalog, store: deps.Store, assembler: deps.Assembler, runtime: deps.Runtime,
			promptClock:       promptClock{timeout: deps.Timeout, source: deps.TimeoutSource, start: idle.WithTimeout},
			autoResolveSource: deps.AutoResolveSource,
			channelOwners:     owners, home: deps.Home, homePath: homePath, skills: deps.Skills,
			projects: deps.Projects, defaultProject: deps.DefaultProject, homeProject: deps.HomeProject,
			memory: deps.Memory, attempts: deps.Attempts, artifacts: deps.Artifacts, intents: deps.Intents,
			executions: deps.Executions, tasks: deps.Tasks, node: deps.Node, schedules: deps.Schedules,
			offlineAfter: deps.OfflineAfter, consoleCompletionGuard: deps.ConsoleCompletionGuard,
			nodes: deps.Nodes, planRecoveryOwner: deps.PlanRecoveryOwner,
			plans: deps.Plans, fleet: deps.Fleet, prober: deps.Prober,
			active: map[string]harness.Runner{}, cancels: map[string]*turnEntry{},
			cancelPending: map[string]time.Time{},
		},
	}, nil
}

// placement is where this agent's process belongs. It comes from the
// catalog rather than the saved session so a config change moves the agent
// on the next /new; a live session keeps its own node because its workspace
// and conversation are over there.
func placement(selected agent.Agent) harness.Placement {
	return harness.Placement{Node: selected.Node, Harness: selected.Harness}
}

// ReviveSession clears the taint a crash left on the member's session so a
// resume turn can run against it. The session is exactly as consistent as
// the agent's own disk state, which the agent reloads on session/load.
func (c *Coordinator) ReviveSession(conversationID, agentID string) error {
	return c.store.ClearTaint(conversationID, agentID)
}

// promptClock bounds how long a prompt may go without progress.
type promptClock struct {
	timeout time.Duration
	// source, when set, is read in place of timeout at each prompt; a
	// saved setting never interrupts a live turn.
	source func() time.Duration
	// start begins a prompt's idle clock. New sets it to idle.WithTimeout;
	// only tests in this package replace it.
	start func(context.Context, time.Duration) (idle.Context, func(), func())
}

func (p promptClock) limit() time.Duration {
	if p.source != nil {
		return p.source()
	}
	return p.timeout
}

func (c *Coordinator) newIdleClock(parent context.Context, d time.Duration) (idle.Context, func(), func()) {
	return c.promptClock.start(parent, d)
}

func (c *Coordinator) promptTimeout() time.Duration { return c.promptClock.limit() }

// autoResolves is whether a merge conflict goes to an agent unasked.
func (c *Coordinator) autoResolves() bool {
	return c.autoResolveSource != nil && c.autoResolveSource()
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
	// Freeze the default language for this request as well as explicitly
	// localized requests. A settings update must not switch it mid-reply.
	c = c.localized(c.text.Locale())
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
	return c.prompt(ctx, req, selected, prompt)
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

func (r Request) Address() channel.Address {
	return channel.Address{Channel: r.Channel, Conversation: r.ConversationID, Message: r.MessageID}
}
