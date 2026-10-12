package turn

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/control"
	"github.com/gopact-ai/steve/internal/conversation"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/identity"
	"github.com/gopact-ai/steve/internal/idle"
	"github.com/gopact-ai/steve/internal/intent"
	"github.com/gopact-ai/steve/internal/memory"
	"github.com/gopact-ai/steve/internal/models"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/roster"
	"github.com/gopact-ai/steve/internal/schedule"
	"github.com/gopact-ai/steve/internal/skills"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/turnapi"
	"github.com/gopact-ai/steve/internal/work"
)

type UserError = turnapi.UserError

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
	// RegisterIdle pauses a prompt's silence clock while node is
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
	// ProbeAll asks every eligible harness, whether or not it has reported
	// a model before.
	ProbeAll(ctx context.Context) []models.Result
}

type Injected = turnapi.Injected
type Result = turnapi.Result

type Coordinator struct {
	*coordinatorState
	text    i18n.Catalog
	ownerID string
	channel string
}

// coordinatorState owns shared execution state. Request-local views replace
// the owner and text catalog; mutexes and runtime state are never copied.
type coordinatorState struct {
	controls    *control.Service
	work        *work.Engine
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
	owners            identity.Owners
	home              home.Loader
	skills            *skills.Live
	// gate is Callbacks.AgentGate: nil when its port cannot be bound.
	gate  AgentGate
	nodes Nodes
	tasks *task.Store

	consoleCompletionGuard ConsoleCompletionGuard

	// modes is how each conversation last reached Steve, for a tool call
	// that has no request to read it from.
	conversations *conversation.Controller
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
	routes      taskRoutes
	afterTurn   func(taskID string)
	turnPreface func(ctx context.Context, taskID string) Preface
	// offlineAfter is how long a turn runs before its completion also earns
	// a plain-text ping; zero keeps Steve quiet.
	offlineAfter time.Duration

	mu              sync.Mutex
	preferenceLocks sync.Map // conversation/agent -> *sync.Mutex
	active          map[string]harness.Runner
	sessionAdmissionState
	cancelPending map[string]time.Time
	skillsLock    int
}

// Deps is everything a Coordinator is built with. New refuses a Deps
// missing anything Deps.required lists; every other field may be zero.
type Deps struct {
	Work      *work.Engine
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
	// Identity is the configured authority shared by input adapters and execution.
	Identity identity.Configuration
	Home     home.Loader
	Skills   *skills.Live
	Projects *project.Store
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

// absent names every dependency in list that is missing.
func absent(list []dependency) []string {
	var missing []string
	for _, dep := range list {
		if dep.absent {
			missing = append(missing, dep.name)
		}
	}
	return missing
}

// New builds a Coordinator from deps, or reports every dependency missing.
func New(deps Deps) (*Coordinator, error) {
	if missing := absent(deps.required()); len(missing) > 0 {
		return nil, fmt.Errorf("turn: missing dependencies: %s", strings.Join(missing, ", "))
	}
	owners, err := identity.NewOwners(deps.Identity)
	if err != nil {
		return nil, fmt.Errorf("turn: %w", err)
	}
	return &Coordinator{
		text:    deps.Text,
		ownerID: deps.Identity.ConsoleOwner,
		coordinatorState: &coordinatorState{
			work:          deps.Work,
			controls:      &control.Service{Owners: owners, Projects: deps.Projects, Tasks: deps.Tasks, Schedules: deps.Schedules, Plans: deps.Plans, DefaultProject: deps.DefaultProject, HomeProject: deps.HomeProject},
			conversations: &conversation.Controller{Store: deps.Store},
			catalog:       deps.Catalog, store: deps.Store, assembler: deps.Assembler, runtime: deps.Runtime,
			promptClock:       promptClock{timeout: deps.Timeout, source: deps.TimeoutSource, start: idle.WithTimeout},
			autoResolveSource: deps.AutoResolveSource,
			owners:            owners, home: deps.Home, skills: deps.Skills,
			projects: deps.Projects, defaultProject: deps.DefaultProject, homeProject: deps.HomeProject,
			memory: deps.Memory, attempts: deps.Attempts, artifacts: deps.Artifacts, intents: deps.Intents,
			executions: deps.Executions, tasks: deps.Tasks, node: deps.Node, schedules: deps.Schedules,
			offlineAfter: deps.OfflineAfter, consoleCompletionGuard: deps.ConsoleCompletionGuard,
			nodes: deps.Nodes, planRecoveryOwner: deps.PlanRecoveryOwner,
			plans: deps.Plans, fleet: deps.Fleet, prober: deps.Prober,
			active:                map[string]harness.Runner{},
			sessionAdmissionState: sessionAdmissionState{cancels: map[string]*turnEntry{}},
			cancelPending:         map[string]time.Time{},
		},
	}, nil
}

// homeDirectory is derived from the configured loader; it cannot drift from
// the home a turn actually injects.
func (c *coordinatorState) homeDirectory() string {
	if dir, ok := c.home.(home.Dir); ok {
		return dir.Path
	}
	return ""
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

// newIdleClock starts a prompt timeout's clock for a prompt on node; the
// clock pauses while the node is away. stop ends it and its registration.
func (c *Coordinator) newIdleClock(parent context.Context, node string) (clock idle.Context, stop func(), touch func()) {
	clock, expire, touch := c.promptClock.start(parent, c.promptTimeout())
	if c.nodes == nil {
		return clock, expire, touch
	}
	unregister := c.nodes.RegisterIdle(node, clock)
	return clock, func() { unregister(); expire() }, touch
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
	if req.Source.ChatType != protocol.ChatGroup || req.Source.Mentioned {
		return ""
	}
	locale := home.LocaleZH
	if c.text.Locale() == i18n.LocaleEN {
		locale = home.LocaleEN
	}
	return home.ListenUnmentioned(locale) + "\n\n"
}

// turnContext is ctx as a turn entry runs under it. A turn is never part
// of the tool call or execution that queued it, like the parent's call
// and the child's execution behind a child's result delivered to its
// parent: it runs on the hub's authority, opens its own scope and binds
// its own grant. Every exported entry that acts on a turn or its task,
// running, relocating, stopping or resolving it, enters through it too.
// Read-only lookups and probes need not: they change nothing.
func turnContext(ctx context.Context) context.Context {
	return execution.WithoutScope(agentmcp.WithoutGrant(ctx))
}

func (c *Coordinator) Handle(ctx context.Context, req Request) (Result, error) {
	ctx = turnContext(ctx)
	c.requestMu.RLock()
	defer c.requestMu.RUnlock()
	var err error
	c, err = c.forChannel(req.Source.Channel)
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
	return c.work.Run(ctx, req, c.handle)
}

func (c *Coordinator) handle(ctx context.Context, req Request) (Result, error) {
	c.mu.Lock()
	retiring := c.retiring[req.Source.ConversationID]
	c.mu.Unlock()
	if retiring {
		return Result{}, UserError{Text: c.text.T(i18n.TurnBusy, protocol.CommandCancel), Cause: ErrConversationBusy}
	}
	// Every arriving message is evidence that someone is present. The
	// offline reminder reads exactly this: nothing arrived while the turn
	// ran, so the person who asked is no longer watching.
	c.noteActivity(req.Source.ConversationID)
	c.rememberMode(req)
	address, parsed := c.ParseInput(req.Input)
	_, explicitSelection := c.catalog.Select(req.Input)
	switchOnly := address != "" && strings.TrimSpace(parsed.Prompt) == ""
	if err := c.controls.CheckCommand(ctx, req, parsed.Command, parsed.Rest, switchOnly); err != nil {
		return Result{}, err
	}
	return work.Dispatch(ctx, req, work.Flow{
		Select: func(req turnapi.Request) (agent.Agent, string, bool, error) {
			var selected agent.Agent
			var prompt string
			var switchOnly bool
			var err error
			if !explicitSelection && parsed.Command != "" {
				selected, prompt, switchOnly, err = c.conversations.Resolve(c.catalog, req.Source.ConversationID, req.Input)
			} else {
				selected, prompt, switchOnly, err = c.conversations.SelectChecked(ctx, c.catalog, req.Source.ConversationID, req.Input, c.controls.Guard(req, project.RoleWrite))
			}
			if err == conversation.ErrNoAgent {
				err = UserError{Text: c.text.T(i18n.NoAgentConfigured)}
			}
			return c.recoveryAgent(req.Source.ConversationID, selected, req.Source.Origin), prompt, switchOnly, err
		},
		Parse: func(prompt string) (string, protocol.Command, string, bool) {
			parsed := ParseInput(prompt)
			return parsed.Prompt, parsed.Command, parsed.Rest, parsed.Interrupt
		},
		Switched: func(selected agent.Agent) Result {
			return Result{AgentID: selected.ID, Text: c.text.T(i18n.Switched, selected.ID)}
		},
		Control: func(ctx context.Context, req turnapi.Request, selected agent.Agent, cmd protocol.Command, rest string) (Result, bool, error) {
			ctx = control.WithRequest(control.WithMutationGuard(ctx, c.controls.Guard(req, project.RoleWrite)), req)
			return c.commands().dispatch(ctx, req, selected, cmd, rest)
		},
		Prompt: c.prompt,
	})
}

func (c *Coordinator) selectAgent(id, input string) (agent.Agent, string, bool, error) {
	selected, prompt, switchOnly, err := c.conversations.Select(c.catalog, id, input)
	if err == conversation.ErrNoAgent {
		err = UserError{Text: c.text.T(i18n.NoAgentConfigured)}
	}
	return selected, prompt, switchOnly, err
}
