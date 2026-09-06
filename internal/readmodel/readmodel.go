// Package readmodel is the system's state as a snapshot, plus a stream of
// changes to it.
//
// It is deliberately not view.Progress. That type is a stream about one turn:
// what this agent is doing right now. This is a snapshot about the whole
// system: which machines are up, what work exists, where it is running, what
// budget is left. A field belongs here when it stays true across turns,
// sessions and hosts, and belongs in view when it only means something inside
// one turn. Merging them would give both surfaces the wrong shape.
//
// The TUI and the dashboard are both renderers over this. Neither has
// privileged access to anything else, which is what keeps them consistent.
package readmodel

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gopact-ai/gopact"
	"github.com/gopact-ai/steve/internal/ability"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/models"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/roster"
	"github.com/gopact-ai/steve/internal/schedule"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/view"
)

// Snapshot is everything a renderer needs in one read.
type Snapshot struct {
	At    time.Time `json:"at"`
	Hub   Hub       `json:"hub"`
	Nodes []Node    `json:"nodes"`
	// Agents is the roster: who exists, where, and whether they can run.
	Agents []Agent `json:"agents"`
	Tasks  []Task  `json:"tasks"`
	Plans  []Plan  `json:"plans"`
	// Attempts are the executions in flight right now: what holds which
	// lease, where.
	Attempts []Attempt `json:"attempts"`
	// Landings are the most recent results brought into a canonical
	// workspace, conflicts included.
	Landings []Landing `json:"landings"`
	// Facts are the rest of what the ledger holds and a person may want
	// to see at a glance: capacity reservations, attestations, replicas,
	// disclosures awaiting the owner, side effects with an unknown
	// outcome, and grants.
	Facts     Facts          `json:"facts"`
	Projects  []Project      `json:"projects"`
	Usage     Usage          `json:"usage"`
	Inbox     []HumanRequest `json:"inbox"`
	Schedules []Schedule     `json:"schedules"`
	Sources   []SourceHealth `json:"sources"`
}

// Facts is the ledger seen from the outside.
type Facts struct {
	Reservations []Reservation `json:"reservations"`
	Attestations []Attestation `json:"attestations"`
	Replicas     []Replica     `json:"replicas"`
	Disclosures  []Disclosure  `json:"disclosures"`
	Effects      []Effect      `json:"effects"`
	Grants       []Grant       `json:"grants"`
	// attentionKnown preserves the completeness of the two inbox queries
	// independently of failures in unrelated fact groups.
	attentionKnown bool
}

type Reservation struct {
	ID        string    `json:"id"`
	Endpoint  string    `json:"endpoint"`
	For       string    `json:"for"`
	Region    string    `json:"region,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
}

type Attestation struct {
	Artifact string    `json:"artifact"`
	Step     string    `json:"step,omitempty"`
	Kind     string    `json:"kind"`
	Verifier string    `json:"verifier"`
	Verdict  string    `json:"verdict"`
	Detail   string    `json:"detail,omitempty"`
	Attempt  string    `json:"attempt"`
	At       time.Time `json:"at"`
}

type Replica struct {
	Artifact   string    `json:"artifact"`
	Node       string    `json:"node"`
	Generation int64     `json:"generation"`
	State      string    `json:"state"`
	Note       string    `json:"note,omitempty"`
	At         time.Time `json:"at"`
}

type Disclosure struct {
	ID        string    `json:"id"`
	Project   string    `json:"project"`
	TaskID    string    `json:"task_id,omitempty"`
	Requester string    `json:"requester"`
	Bytes     int       `json:"bytes"`
	At        time.Time `json:"at"`
}

type Effect struct {
	ID      string    `json:"id"`
	Tool    string    `json:"tool"`
	TaskID  string    `json:"task_id"`
	Attempt string    `json:"attempt"`
	Error   string    `json:"error,omitempty"`
	At      time.Time `json:"at"`
}

type Grant struct {
	Project   string `json:"project"`
	Principal string `json:"principal"`
	Role      string `json:"role"`
	By        string `json:"by"`
}

type Attempt struct {
	Unsettled bool      `json:"unsettled,omitempty"`
	Error     string    `json:"error,omitempty"`
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	State     string    `json:"state"`
	TaskID    string    `json:"task_id,omitempty"`
	Project   string    `json:"project"`
	Agent     string    `json:"agent,omitempty"`
	Node      string    `json:"node,omitempty"`
	Scope     string    `json:"scope"`
	Workspace string    `json:"workspace,omitempty"`
	Leases    []string  `json:"leases,omitempty"`
	StartedAt time.Time `json:"started_at"`
	// Requires is what the work asked of the machine; Admission the
	// machine's final word on it before the attempt ran.
	Requires  []string           `json:"requires,omitempty"`
	Admission *ability.Admission `json:"admission,omitempty"`
}

// Condition is one of an agent's requirements as the machine meets it.
type Condition struct {
	Atom   string `json:"atom"`
	Met    bool   `json:"met"`
	Code   string `json:"code,omitempty"`
	Detail string `json:"detail,omitempty"`
}

type Landing struct {
	ID       string    `json:"id"`
	Project  string    `json:"project"`
	Artifact string    `json:"artifact"`
	State    string    `json:"state"`
	Paths    int       `json:"paths"`
	Error    string    `json:"error,omitempty"`
	At       time.Time `json:"at"`
}

type Hub struct {
	Node         string    `json:"node"`
	Started      time.Time `json:"started"`
	Capabilities []string  `json:"capabilities,omitempty"`
	// Level is the hub machine's own data level; Advert is what it can
	// run, checked the way a node checks itself. Sources.HubAdvert fills
	// it fresh for every snapshot.
	Level   string          `json:"level,omitempty"`
	Advert  nodewire.Advert `json:"advert"`
	Version string          `json:"version,omitempty"`
}

// Roles a node can have. The hub is a node with the coordinating role,
// not a place of its own.
const (
	RoleHub    = "hub"
	RoleWorker = "worker"
)

type Node struct {
	Name string `json:"name"`
	Role string `json:"role"`
	// Version is the steve build the machine runs.
	Version string `json:"version,omitempty"`
	// Addr is where the hub dials the node; Host and IPs are what the
	// machine says about itself.
	Addr         string    `json:"addr,omitempty"`
	Host         string    `json:"host,omitempty"`
	IPs          []string  `json:"ips,omitempty"`
	Up           bool      `json:"up"`
	Since        time.Time `json:"since,omitzero"`
	OS           string    `json:"os,omitempty"`
	Arch         string    `json:"arch,omitempty"`
	Capabilities []string  `json:"capabilities,omitempty"`
	Harnesses    []Harness `json:"harnesses,omitempty"`
	LastError    string    `json:"last_error,omitempty"`
	Level        string    `json:"level,omitempty"`
	Region       string    `json:"region,omitempty"`
	// Snapshot is what the machine says it can do, entry by entry, with
	// the evidence and coverage behind each.
	Snapshot *ability.Snapshot `json:"snapshot,omitempty"`
	// Features are the protocol features the machine negotiated; a node
	// without execution_admission.v1 cannot be asked for a final word.
	Features []string `json:"features,omitempty"`
	// Health is the machine's room to work, as of its last advert.
	Health *nodewire.Health `json:"health,omitempty"`
}

type Harness struct {
	ID    string `json:"id"`
	Slots int    `json:"slots,omitempty"`
	// Version is the adapter as it introduced itself over ACP.
	Version string `json:"version,omitempty"`
	// Model is what the harness was last seen running here; Models what
	// it offers, declared or observed.
	Model   string   `json:"model,omitempty"`
	Models  []string `json:"models,omitempty"`
	Missing string   `json:"missing,omitempty"`
}

type Agent struct {
	ID      string `json:"id"`
	Node    string `json:"node,omitempty"`
	Harness string `json:"harness"`
	// Models are what the harness offers here, by observation or config;
	// Repair names the agent that could fix this one when it is blocked
	// by a harness missing on its machine.
	Models   []string `json:"models,omitempty"`
	Repair   string   `json:"repair,omitempty"`
	Model    string   `json:"model,omitempty"`
	Eligible bool     `json:"eligible"`
	Why      string   `json:"why,omitempty"`
	Requires []string `json:"requires,omitempty"`
	// Preferred is the model the agent's configuration pins, applied at
	// session open; Observed is what the harness was last seen running.
	// Conditions are Requires judged against the machine, one by one.
	Preferred string `json:"preferred,omitempty"`
	Observed  string `json:"observed,omitempty"`
	// Options are the other selectors the agent pins; Selectors every
	// selector its harness exposed last time; About what it is for.
	Options    map[string]string `json:"options,omitempty"`
	Selectors  []models.Selector `json:"selectors,omitempty"`
	About      string            `json:"about,omitempty"`
	Conditions []Condition       `json:"conditions,omitempty"`
	MCPServers []string          `json:"mcp_servers,omitempty"`
	Default    bool              `json:"default,omitempty"`
	Level      string            `json:"level,omitempty"`
	Slots      int               `json:"slots,omitempty"`
	Region     string            `json:"region,omitempty"`
	// Activities are known live attempts with the latest thing each was
	// seen doing; Busy is their count. Neither proves absence when the
	// activity query is incomplete.
	Activities []Activity `json:"activities,omitempty"`
	Busy       int        `json:"busy,omitempty"`
	// ActivityKnown is false when the live-attempt query is incomplete.
	ActivityKnown *bool `json:"activity_known,omitempty"`
	// Snapshot is the machine's, plus the models this harness was seen
	// running: what a requirement is matched against.
	Snapshot *ability.Snapshot `json:"snapshot,omitempty"`
}

type Task struct {
	ID         string   `json:"id"`
	Goal       string   `json:"goal"`
	Title      string   `json:"title,omitempty"`
	Priority   string   `json:"priority,omitempty"`
	Labels     []string `json:"labels,omitempty"`
	ArchivedAt string   `json:"archived_at,omitempty"`
	State      string   `json:"state"`
	Member     string   `json:"member,omitempty"`
	NodeID     string   `json:"node,omitempty"`
	Parent     string   `json:"parent,omitempty"`
	// Children makes the tree explicit so a renderer does not have to build
	// it — the tree is the whole debugging story for delegated work.
	Children  []string  `json:"children,omitempty"`
	Turns     int       `json:"turns"`
	MaxTurns  int       `json:"max_turns"`
	Elapsed   string    `json:"elapsed"`
	MaxElapse string    `json:"max_elapsed"`
	UpdatedAt time.Time `json:"updated_at"`
	PlanID    string    `json:"plan_id,omitempty"`
	// Tokens and Seconds are what the task has spent across attempts.
	Tokens  Tokens `json:"tokens"`
	Seconds int64  `json:"seconds"`
	Model   string `json:"model,omitempty"`
	// Channel, ProjectID and Origin say where the task was asked, in
	// which project, and by what (chat, plan, schedule, delegate).
	Channel   string `json:"channel,omitempty"`
	ProjectID string `json:"project_id,omitempty"`
	Origin    string `json:"origin,omitempty"`
	Requester string `json:"requester,omitempty"`
	// AttemptRows are the task's turns as the task store caches them; the
	// ledger's attempt records are the authority for what they cost.
	AttemptRows []AttemptRow `json:"attempt_rows,omitempty"`
	// Four axes, decided here and rolled up from every descendant task:
	// Lifecycle is the task's own state; Execution says whether an
	// attempt is live (idle|running|unknown); Attention counts known requests
	// waiting; Lane is the board column that follows from the three.
	Lifecycle string `json:"lifecycle"`
	Execution string `json:"execution"`
	Attention int    `json:"attention"`
	Lane      string `json:"lane"`
}

type Plan struct {
	ID      string `json:"id"`
	TaskID  string `json:"task_id"`
	Rev     int    `json:"rev"`
	Goal    string `json:"goal"`
	By      string `json:"by"`
	Because string `json:"because"`
	Steps   []Step `json:"steps"`
}

type Step struct {
	ID       string   `json:"id"`
	Goal     string   `json:"goal"`
	State    string   `json:"state"`
	Agent    string   `json:"agent,omitempty"`
	Node     string   `json:"node,omitempty"`
	Needs    []string `json:"needs,omitempty"`
	Merge    []string `json:"merge,omitempty"`
	Requires []string `json:"requires,omitempty"`
	Attempts int      `json:"attempts,omitempty"`
	Verify   string   `json:"verify,omitempty"`
	Error    string   `json:"error,omitempty"`
	// Context is what this step's agent was actually given. It is here
	// because "what did it see?" is the first question when a delegated
	// step goes wrong, and a payload nobody can inspect is a payload nobody
	// can debug.
	Context   *StepContext `json:"context,omitempty"`
	Usage     *StepUsage   `json:"usage,omitempty"`
	StartedAt time.Time    `json:"started_at,omitzero"`
	EndedAt   time.Time    `json:"ended_at,omitzero"`
}

type StepContext struct {
	Goal      string   `json:"goal"`
	Ancestry  []string `json:"ancestry,omitempty"`
	Refs      []string `json:"refs,omitempty"`
	Findings  []string `json:"findings,omitempty"`
	Facts     []string `json:"facts,omitempty"`
	TurnsLeft int      `json:"turns_left,omitempty"`
	Bytes     int      `json:"bytes"`
}

// Sources are the live stores the model reads. Each is optional: a hub with
// no plans still reports its nodes.
// ScheduleSource lists standing work; the schedule store satisfies it.
type ScheduleSource interface {
	List(conversationID string) []schedule.Job
}

// Models is what harnesses were seen running; the models package's Book
// satisfies it.
type Models interface {
	Get(node, harness string) (models.Observation, bool)
}

type Sources struct {
	Hub    Hub
	Roster *roster.Roster
	Nodes  NodeSource
	Tasks  *task.Store
	Plans  PlanSource
	// Ledger is where attempts and landings are read from.
	Ledger LedgerSource
	// HubAdvert describes the hub machine now, not at startup: a harness
	// installed since is seen by the next snapshot.
	HubAdvert func() nodewire.Advert
	// Repos answers what repositories a workspace's directory holds, by
	// workspace id, from a cache the hub keeps; HomeProject and
	// DefaultProject name the two special projects.
	Repos          func(workspaceID string) []nodewire.Repo
	HomeProject    string
	DefaultProject string
	// Models is the book of observed models, for node harnesses that
	// declare none.
	Models Models
	// Schedules is standing work; Observations is where connectivity
	// facts are kept for history.
	Schedules    ScheduleSource
	Observations ledger.Doc
}

// LedgerSource is what the read model needs from the ledger-backed
// services: the live attempts and a project's landings.
type LedgerSource interface {
	LiveAttempts(ctx context.Context) ([]Attempt, error)
	RecentLandings(ctx context.Context) ([]Landing, error)
	Facts(ctx context.Context) (Facts, error)
	// ProjectList lists every project, for the page and the context bar.
	ProjectList(ctx context.Context) ([]project.Project, error)
	// ClosedAttempts are every attempt that reached a terminal state: the
	// authority on spend. Events pages the journal for history.
	ClosedAttempts(ctx context.Context) ([]attempt.Record, error)
	Events(ctx context.Context, before int64, limit int) ([]ledger.Event, error)
}

type NodeSource interface {
	Statuses() []node.Status
}

type PlanSource interface {
	List() []plan.Plan
}

// Model serves snapshots and a change stream.
type Model struct {
	src Sources

	mu   sync.Mutex
	subs map[int]chan Event
	next int
	// recent keeps the last events so a renderer that attaches mid-flight
	// has something to show immediately instead of a blank screen.
	recent       []Event
	throttle     map[string]throttled
	activity     map[string]Activity
	observations []Observation
}

// Event is one change worth waking a renderer for.
type Event struct {
	At   time.Time `json:"at"`
	Kind string    `json:"kind"`
	// Seq is gopact's per-run sequence. Delivery is at-least-once, so a
	// consumer that cares about exactly-once dedupes on (RunID, Seq).
	Seq    int64  `json:"seq,omitempty"`
	RunID  string `json:"run_id,omitempty"`
	TaskID string `json:"task_id,omitempty"`
	PlanID string `json:"plan_id,omitempty"`
	StepID string `json:"step_id,omitempty"`
	State  string `json:"state,omitempty"`
	// Conversation and Text carry the console's traffic: what was sent
	// from the page, what came back, and milestones agents posted. A run
	// event is stamped with the conversation its task came from, so a
	// page can follow one conversation's work.
	Conversation string `json:"conversation,omitempty"`
	Text         string `json:"text,omitempty"`
	Format       string `json:"format,omitempty"`
	// Progress is what an agent is doing right now: console.progress for
	// a chat turn, step.progress for a plan step.
	Progress *consoleapi.Progress `json:"progress,omitempty"`
	// Step is the child's snapshot; console.step also names its owning reply.
	Step *consoleapi.StepProcess `json:"step,omitempty"`
	// ReplyID names the console line a console.* event is about.
	ReplyID    string `json:"reply_id,omitempty"`
	ExchangeID string `json:"exchange_id,omitempty"`
	Title      string `json:"title,omitempty"`
	Rev        int    `json:"rev,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

const recentKept = 200

func New(src Sources) *Model {
	return &Model{src: src, subs: map[int]chan Event{}}
}

// AttemptRow is one turn of a task: who ran it, on what, for how long,
// and what it reported spending. Reported false means the harness said
// nothing about tokens, which is not the same as zero.
type AttemptRow struct {
	Day      string    `json:"day"`
	Agent    string    `json:"agent"`
	Node     string    `json:"node,omitempty"`
	Model    string    `json:"model,omitempty"`
	Outcome  string    `json:"outcome,omitempty"`
	Started  time.Time `json:"started"`
	Seconds  int64     `json:"seconds"`
	Tokens   Tokens    `json:"tokens"`
	Reported bool      `json:"reported"`
}

// StepUsage is a plan step's spend as the harness reported it.
type StepUsage struct {
	Day     string `json:"day"`
	Model   string `json:"model,omitempty"`
	Tokens  Tokens `json:"tokens"`
	Seconds int64  `json:"seconds"`
}

// Activity is what an agent is doing right now, from its progress stream:
// which task and step, the latest tool call, and since when.
type Activity struct {
	Agent        string    `json:"agent"`
	AttemptID    string    `json:"attempt_id,omitempty"`
	Kind         string    `json:"kind,omitempty"`
	Workspace    string    `json:"workspace,omitempty"`
	TaskID       string    `json:"task_id,omitempty"`
	StepID       string    `json:"step_id,omitempty"`
	Conversation string    `json:"conversation,omitempty"`
	Tool         string    `json:"tool,omitempty"`
	Detail       string    `json:"detail,omitempty"`
	Since        time.Time `json:"since"`
	At           time.Time `json:"at"`
}

// Tokens is a usage total, in the task store's own shape.
type Tokens struct {
	Input       int64 `json:"input,omitempty"`
	Output      int64 `json:"output,omitempty"`
	CachedRead  int64 `json:"cached_read,omitempty"`
	CachedWrite int64 `json:"cached_write,omitempty"`
	Total       int64 `json:"total,omitempty"`
	// Context is the context window in use at the last report: what the
	// adapters say when they report no token counts. Summed over
	// attempts it is a size, not a spend; shown as such.
	Context int64 `json:"context,omitempty"`
}

func (t Tokens) add(o Tokens) Tokens {
	return Tokens{Input: t.Input + o.Input, Output: t.Output + o.Output, CachedRead: t.CachedRead + o.CachedRead, CachedWrite: t.CachedWrite + o.CachedWrite, Total: t.Total + o.Total, Context: t.Context + o.Context}
}

// Usage is what the fleet has spent: per day, per agent, per model, from
// every closed attempt on record. Tokens and wall time only —
// money needs a price table this system does not have.
type Usage struct {
	ByDay    []UsageRow             `json:"by_day"`
	ByAgent  []UsageRow             `json:"by_agent"`
	ByModel  []UsageRow             `json:"by_model"`
	Total    UsageRow               `json:"total"`
	Timezone string                 `json:"timezone"`
	Periods  map[string]UsagePeriod `json:"periods"`
}

// UsagePeriod counts attempts by their start time in the hub's calendar.
// To is the snapshot time; the last bucket can be incomplete. Series keys
// are RFC3339 bucket starts with their local UTC offset, including during DST.
type UsagePeriod struct {
	From     time.Time  `json:"from"`
	To       time.Time  `json:"to"`
	Interval string     `json:"interval"`
	Series   []UsageRow `json:"series"`
	ByAgent  []UsageRow `json:"by_agent"`
	ByModel  []UsageRow `json:"by_model"`
	Total    UsageRow   `json:"total"`
}

type UsageRow struct {
	Key      string `json:"key"`
	Tokens   Tokens `json:"tokens"`
	Seconds  int64  `json:"seconds"`
	Attempts int    `json:"attempts"`
	// Unreported counts attempts whose harness said nothing about tokens.
	Unreported int `json:"unreported,omitempty"`
}

// HumanRequest is one thing only a person can settle, projected from the
// operation that is waiting: a disclosure, an effect with an unknown
// outcome. The operation stays the authority; this is how the inbox
// shows it, with the choices that are actually available.
type HumanRequest struct {
	AttemptID  string    `json:"attempt_id,omitempty"`
	Node       string    `json:"node,omitempty"`
	Workspace  string    `json:"workspace,omitempty"`
	ID         string    `json:"id"`
	Type       string    `json:"type"`
	Source     string    `json:"source"`
	ProjectID  string    `json:"project_id,omitempty"`
	TaskID     string    `json:"task_id,omitempty"`
	Summary    string    `json:"summary"`
	Choices    []Choice  `json:"choices"`
	CreatedAt  time.Time `json:"created_at"`
	Resolvable bool      `json:"resolvable"`
}

// Choice is one answer to a request, as the command that gives it.
type Choice struct {
	Label   string `json:"label"`
	Command string `json:"command"`
	Danger  bool   `json:"danger,omitempty"`
}

// Schedule is standing or one-off work the gateway will start by itself.
type Schedule struct {
	ID           string    `json:"id"`
	Conversation string    `json:"conversation"`
	Agent        string    `json:"agent,omitempty"`
	Prompt       string    `json:"prompt"`
	Spec         string    `json:"spec"`
	NextAt       time.Time `json:"next_at"`
	LastAt       time.Time `json:"last_at,omitzero"`
	Runs         int       `json:"runs"`
	State        string    `json:"state,omitempty"`
	Error        string    `json:"error,omitempty"`
	PendingKey   string    `json:"pending_key,omitempty"`
}

// SourceHealth says whether a source contributed to the snapshot, so a
// page can tell "none" from "could not read".
type SourceHealth struct {
	Name  string `json:"name"`
	Wired bool   `json:"wired"`
	Error string `json:"error,omitempty"`
}

// HistoryEntry is one thing that happened, in words, with the record
// behind it: a ledger transition or a connectivity observation.
type HistoryEntry struct {
	At        time.Time `json:"at"`
	Seq       int64     `json:"seq,omitempty"`
	Kind      string    `json:"kind"`
	Subject   string    `json:"subject,omitempty"`
	Text      string    `json:"text"`
	Actor     string    `json:"actor,omitempty"`
	Operation string    `json:"operation,omitempty"`
	From      string    `json:"from,omitempty"`
	To        string    `json:"to,omitempty"`
}

// Observation is a connectivity fact worth remembering: a machine came
// up with a build, went down with a reason, a probe answered. Kept in a
// ledger document so history survives the process.
type Observation struct {
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"`
	Subject string    `json:"subject"`
	Text    string    `json:"text"`
}

// Project is where work happens: a directory on one machine, with a data
// level and a repo mode, and the agents that could work it right now —
// judged by the same rule a turn is judged by.
type Project struct {
	ID          string   `json:"id"`
	Node        string   `json:"node"`
	Path        string   `json:"path"`
	Level       string   `json:"level"`
	Repo        string   `json:"repo"`
	DefaultRole string   `json:"default_role,omitempty"`
	Agents      []string `json:"agents"`
	// Repos are the git repositories inside the project's home directory,
	// as its machine last reported them; Node, Path and Repos describe the
	// home, Agents is the union over every workspace. Home marks Steve's
	// own home — the owner's private conversation space, not a codebase;
	// Default marks what a fresh conversation binds to.
	Repos   []nodewire.Repo `json:"repos"`
	Home    bool            `json:"home,omitempty"`
	Default bool            `json:"default,omitempty"`
	// Workspaces are where the project is: its home first, then its
	// copies on other machines.
	Workspaces []Workspace `json:"workspaces"`
}

// Workspace is one place a project is: a directory on a machine, what it
// holds, and which agents can work in it.
type Workspace struct {
	ID     string `json:"id"`
	Node   string `json:"node"`
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Origin string `json:"origin,omitempty"`
	Source string `json:"source,omitempty"`
	// State is ready, provisioning or failed (a copy being cloned, or one
	// that could not be); Error says why it failed. Busy says an attempt
	// is known to run here; a false value proves idle only if ActivityKnown.
	State         string `json:"state,omitempty"`
	Error         string `json:"error,omitempty"`
	Busy          bool   `json:"busy,omitempty"`
	ActivityKnown *bool  `json:"activity_known,omitempty"`
	// Repos is nil until the machine has been asked.
	Repos  []nodewire.Repo `json:"repos"`
	Agents []string        `json:"agents"`
}

// Answers and tool output are previews. Reasoning is already bounded by
// the collector and must retain its head, tail and explicit omission marker.
const (
	answerKept   = 8000
	toolTextKept = 3000
	toolsKept    = 60
)

// FromProgress cuts a turn's progress down to what is worth sending.
func FromProgress(p view.Progress) consoleapi.Progress {
	out := consoleapi.Progress{
		Agent: p.Agent, Node: p.Settings.Node, Model: p.Settings.Model,
		Reasoning: p.Reasoning, Answer: tailText(p.Answer, answerKept),
	}
	limit := toolsKept
	if len(p.Timeline) > 0 {
		// Every timeline reference needs a corresponding tool detail.
		limit = 0
	}
	out.Tools = toolCalls(p.Tools, 0, limit)
	for _, s := range p.Timeline {
		out.Timeline = append(out.Timeline, consoleapi.Span{Kind: s.Kind, Text: s.Text, Tool: s.Tool, At: s.At})
	}
	for _, s := range p.Plan {
		out.Plan = append(out.Plan, consoleapi.PlanLine{Text: s.Text, Status: string(s.Status)})
	}
	return out
}

// The platform's own tools are recognised whatever a harness calls them
// — claude-code says mcp__steve__steve_fleet, codex mcp.steve.steve_fleet
// — and shown by their label, as kind "platform". The catalogue comes
// from the messaging server itself, so a new tool needs no page change.
var platform struct {
	mu     sync.RWMutex
	server string
	titles map[string]string
}

// SetPlatformTools installs the messaging server's name and tool labels.
func SetPlatformTools(server string, titles map[string]string) {
	platform.mu.Lock()
	defer platform.mu.Unlock()
	platform.server = strings.ToLower(server)
	platform.titles = map[string]string{}
	for name, title := range titles {
		platform.titles[strings.ToLower(name)] = title
	}
}

// platformTool says whether a harness's tool name is one of the
// platform's, and what to call it.
func platformTool(raw string) (name, title string, ok bool) {
	platform.mu.RLock()
	defer platform.mu.RUnlock()
	if platform.server == "" || len(platform.titles) == 0 {
		return "", "", false
	}
	lower := strings.ToLower(strings.TrimSpace(raw))
	if title, found := platform.titles[lower]; found {
		return lower, title, true
	}
	// Split on every separator a harness uses between server and tool,
	// but never on a single underscore: that is inside the tool's name.
	parts := strings.FieldsFunc(strings.ReplaceAll(lower, "__", "\x00"), func(r rune) bool {
		return r == '\x00' || r == '.' || r == '/' || r == ':'
	})
	for i, p := range parts {
		if p != platform.server || i+1 >= len(parts) {
			continue
		}
		tail := strings.Join(parts[i+1:], "_")
		if title, found := platform.titles[tail]; found {
			return tail, title, true
		}
	}
	return "", "", false
}

func toolCalls(tools []view.Tool, depth, limit int) []consoleapi.ToolCall {
	var out []consoleapi.ToolCall
	for _, t := range tools {
		if limit > 0 && len(out) >= limit {
			break
		}
		call := consoleapi.ToolCall{
			ID: t.ID, Kind: t.Kind, Name: t.Name, Detail: t.Detail, Status: string(t.Status),
			Input: headText(t.Input, toolTextKept), Output: headText(t.Output, toolTextKept),
		}
		if name, title, ok := platformTool(t.Name); ok {
			call.Kind, call.Name, call.Detail = "platform", name, title
		}
		out = append(out, call)
		if depth < 2 {
			out = append(out, toolCalls(t.Children, depth+1, limit)...)
		}
	}
	return out
}

func tailText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

func headText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// FromStepProgress uses the same projection for live events and durable steps.
func FromStepProgress(id string, p consoleapi.Progress, info consoleapi.StepInfo) consoleapi.StepProcess {
	if info.Answer == "" {
		info.Answer = p.Answer
	}
	return consoleapi.StepProcess{ID: id, Agent: p.Agent, Node: p.Node, Model: p.Model,
		Reasoning: p.Reasoning, Tools: p.Tools, Plan: p.Plan, Timeline: p.Timeline, StepInfo: info}
}

// DelegateProgress publishes what a delegated child is doing, as a step
// of its parent's conversation: the page shows it as a card under the
// parent's delegate call. A terminal state is never throttled — the
// last word must land.
func (m *Model) DelegateProgress(childTaskID, agent, node string, info consoleapi.StepInfo, p view.Progress) {
	stepID := "#" + childTaskID
	key := "delegate/" + stepID
	terminal := info.State == "done" || info.State == "failed"
	if terminal {
		m.mu.Lock()
		delete(m.throttle, key)
		m.mu.Unlock()
	} else {
		signature := fmt.Sprintf("%d", len(p.Tools))
		if n := len(p.Tools); n > 0 {
			signature += "/" + string(p.Tools[n-1].Status)
		}
		m.mu.Lock()
		if m.throttle == nil {
			m.throttle = map[string]throttled{}
		}
		last := m.throttle[key]
		now := time.Now()
		if now.Sub(last.at) < progressEvery && last.signature == signature {
			m.mu.Unlock()
			return
		}
		m.throttle[key] = throttled{at: now, signature: signature}
		m.mu.Unlock()
	}
	progress := FromProgress(p)
	if progress.Agent == "" {
		progress.Agent = agent
	}
	if progress.Node == "" {
		progress.Node = node
	}
	info.Kind = "delegate"
	step := FromStepProgress(stepID, progress, info)
	m.Publish(Event{
		Kind: "delegate.progress", TaskID: childTaskID, StepID: stepID,
		Conversation: m.conversationOf(childTaskID), Progress: &progress, Step: &step,
	})
}

// StepProgress publishes what a plan step's agent is doing, stamped with
// the conversation the plan's task came from. It is throttled per step:
// a token stream is not a change worth a page repaint each time, but a
// tool call starting or finishing always is.
func (m *Model) StepProgress(taskID, planID, stepID, agent, node string, p view.Progress) {
	key := planID + "/" + stepID
	signature := fmt.Sprintf("%d", len(p.Tools))
	if n := len(p.Tools); n > 0 {
		signature += "/" + string(p.Tools[n-1].Status)
	}
	m.mu.Lock()
	if m.throttle == nil {
		m.throttle = map[string]throttled{}
	}
	last := m.throttle[key]
	now := time.Now()
	if now.Sub(last.at) < progressEvery && last.signature == signature {
		m.mu.Unlock()
		return
	}
	m.throttle[key] = throttled{at: now, signature: signature}
	m.mu.Unlock()
	progress := FromProgress(p)
	if progress.Agent == "" {
		progress.Agent = agent
	}
	if progress.Node == "" {
		progress.Node = node
	}
	m.Publish(Event{
		Kind: "step.progress", TaskID: taskID, PlanID: planID, StepID: stepID,
		Conversation: m.conversationOf(taskID), Progress: &progress,
	})
}

type throttled struct {
	at        time.Time
	signature string
}

// progressEvery bounds how often one step's token stream repaints a page.
const progressEvery = 400 * time.Millisecond

// TaskChanged tells the pages a task moved: a light notice with the id
// and the conversation it belongs to, so a page invalidates rather than
// recomputes everything.
func (m *Model) TaskChanged(taskID string) {
	m.Publish(Event{Kind: "task.changed", TaskID: taskID, Conversation: m.conversationOf(taskID)})
}

// conversationOf is the channel a task was asked in, "" when unknown.
func (m *Model) conversationOf(taskID string) string {
	if taskID == "" || m.src.Tasks == nil {
		return ""
	}
	if t, ok := m.src.Tasks.Get(taskID); ok {
		return t.Channel
	}
	return ""
}

// taskOfPlan is the task a plan belongs to, through the plan store.
func (m *Model) taskOfPlan(planID string) string {
	if planID == "" || m.src.Plans == nil {
		return ""
	}
	for _, p := range m.src.Plans.List() {
		if p.ID == planID {
			return p.TaskID
		}
	}
	return ""
}

// Emit makes the model a gopact.EventSink, so workflow node transitions reach
// the renderers as they happen rather than on the next poll.
//
// gopact's own run log remains the durable authority; this is the live
// projection for screens. A renderer that misses an event re-reads the
// snapshot, which is why dropping is preferable to blocking the runtime.
func (m *Model) Emit(_ context.Context, ev gopact.Event) error {
	taskID := m.taskOfPlan(ev.DefinitionID)
	m.Publish(Event{
		At:     ev.Timestamp,
		Kind:   ev.Type,
		RunID:  ev.RunID,
		TaskID: taskID,
		PlanID: ev.DefinitionID,
		StepID: ev.NodeID,
		Seq:    ev.Sequence,
		Detail: ev.Summary,

		Conversation: m.conversationOf(taskID),
	})
	return nil
}

func (m *Model) Publish(ev Event) {
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	m.mu.Lock()
	m.noteActivity(ev)
	m.recent = append(m.recent, ev)
	if len(m.recent) > recentKept {
		m.recent = m.recent[len(m.recent)-recentKept:]
	}
	// Sent under the lock, so a subscriber cancelling — which closes its
	// channel — cannot race a send. The send never blocks: a renderer that
	// cannot keep up misses events and re-reads the snapshot, which is
	// the right tradeoff.
	for _, ch := range m.subs {
		select {
		case ch <- ev:
		default:
		}
	}
	m.mu.Unlock()
}

// Observe records a connectivity fact and tells the page. The list is
// kept in the ledger document so a restart does not forget it.
func (m *Model) Observe(kind, subject, text string) {
	obs := Observation{At: time.Now().UTC(), Kind: kind, Subject: subject, Text: text}
	m.mu.Lock()
	m.observations = append(m.observations, obs)
	if len(m.observations) > observationsKept {
		m.observations = m.observations[len(m.observations)-observationsKept:]
	}
	doc, list := m.src.Observations, append([]Observation(nil), m.observations...)
	m.mu.Unlock()
	if doc != nil {
		if raw, err := json.Marshal(list); err == nil {
			if err := doc.Save(raw); err != nil {
				log.Printf("readmodel: save observations: %v", err)
			}
		}
	}
	m.Publish(Event{Kind: "observe." + kind, Detail: text, Text: subject})
}

const observationsKept = 1000

// LoadObservations brings back what an earlier process observed.
func (m *Model) LoadObservations() error {
	if m.src.Observations == nil {
		return nil
	}
	raw, ok, err := m.src.Observations.Load()
	if err != nil || !ok || len(raw) == 0 {
		return err
	}
	var list []Observation
	if err := json.Unmarshal(raw, &list); err != nil {
		return err
	}
	m.mu.Lock()
	m.observations = list
	m.mu.Unlock()
	return nil
}

// History pages what happened, newest first: ledger transitions in
// words, merged with connectivity observations. before is the ledger
// sequence to page from (0 = the end).
func (m *Model) History(ctx context.Context, before int64, limit int) ([]HistoryEntry, int64, error) {
	if limit <= 0 || limit > 200 {
		limit = 60
	}
	var out []HistoryEntry
	var next int64
	if m.src.Ledger != nil {
		events, err := m.src.Ledger.Events(ctx, before, limit)
		if err != nil {
			return nil, 0, err
		}
		for _, ev := range events {
			out = append(out, HistoryEntry{
				At: ev.At, Seq: ev.Seq, Kind: "ledger", Subject: ev.OperationID, Operation: ev.OperationID,
				From: ev.From, To: ev.To, Actor: ev.Actor, Text: describeEvent(ev),
			})
			next = ev.Seq
		}
	}
	// Observations have no sequence; they slot in by time. The first page
	// takes everything newer than its oldest ledger entry; a later page
	// takes what falls between its oldest and its newest.
	var floor, ceiling time.Time
	if len(out) > 0 {
		floor = out[len(out)-1].At
		if before > 0 {
			ceiling = out[0].At
		}
	}
	m.mu.Lock()
	for _, o := range m.observations {
		if !o.At.After(floor) && !floor.IsZero() {
			continue
		}
		if !ceiling.IsZero() && o.At.After(ceiling) {
			continue
		}
		out = append(out, HistoryEntry{At: o.At, Kind: "observe." + o.Kind, Subject: o.Subject, Text: o.Text})
	}
	m.mu.Unlock()
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out, next, nil
}

// describeEvent puts a ledger transition into words. The operation id's
// prefix says what kind of thing moved; the states say what happened.
func describeEvent(ev ledger.Event) string {
	kind := "operation"
	switch {
	case strings.HasPrefix(ev.OperationID, "att-"):
		kind = "attempt"
	case strings.HasPrefix(ev.OperationID, "landing-"), strings.HasPrefix(ev.OperationID, "land-"):
		kind = "landing"
	case strings.HasPrefix(ev.OperationID, "disc-"):
		kind = "disclosure"
	case strings.HasPrefix(ev.OperationID, "intent-"), strings.HasPrefix(ev.OperationID, "eff-"):
		kind = "effect"
	}
	who := ev.Actor
	if who == "" {
		who = "steve"
	}
	if ev.From == "" {
		return fmt.Sprintf("%s %s opened as %s by %s", kind, ev.OperationID, ev.To, who)
	}
	return fmt.Sprintf("%s %s: %s → %s by %s", kind, ev.OperationID, ev.From, ev.To, who)
}

// noteActivity keeps, per agent, what its latest progress says it is
// doing. Called under the lock. A reply or a step's end is not an event
// here; the page judges staleness by At and by the attempts in flight.
func (m *Model) noteActivity(ev Event) {
	if ev.Progress == nil || ev.Progress.Agent == "" {
		return
	}
	if ev.Kind != "console.progress" && ev.Kind != "step.progress" && ev.Kind != "delegate.progress" {
		return
	}
	if m.activity == nil {
		m.activity = map[string]Activity{}
	}
	prev := m.activity[ev.Progress.Agent]
	next := Activity{Agent: ev.Progress.Agent, TaskID: ev.TaskID, StepID: ev.StepID, Conversation: ev.Conversation, At: ev.At, Since: prev.Since}
	if prev.TaskID != next.TaskID || prev.StepID != next.StepID || prev.Since.IsZero() {
		next.Since = ev.At
	}
	for _, t := range ev.Progress.Tools {
		next.Tool, next.Detail = t.Kind, t.Name
		if t.Status == "running" {
			break
		}
	}
	m.activity[ev.Progress.Agent] = next
}

// Subscribe returns a channel of changes and a cancel function.
func (m *Model) Subscribe(ctx context.Context) (<-chan Event, func()) {
	ch := make(chan Event, 64)
	m.mu.Lock()
	id := m.next
	m.next++
	m.subs[id] = ch
	m.mu.Unlock()
	stop := func() {
		m.mu.Lock()
		if existing, ok := m.subs[id]; ok {
			delete(m.subs, id)
			close(existing)
		}
		m.mu.Unlock()
	}
	go func() {
		<-ctx.Done()
		stop()
	}()
	return ch, stop
}

// Recent returns the buffered change history, oldest first.
func (m *Model) Recent() []Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Event{}, m.recent...)
}
