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
	"fmt"
	"log/slog"
	"strconv"
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
	// Base state is live work plus ancestor closure and recent closed work,
	// not a complete historical inventory. Query/detail APIs expose the rest.
	TaskCoverage task.Coverage `json:"task_coverage"`
	PlanCoverage PlanCoverage  `json:"plan_coverage"`
	// Attempts are the executions in flight right now: what holds which
	// lease, where.
	Attempts []Attempt `json:"attempts"`
	// Landings are the most recent results brought into a canonical
	// workspace, conflicts included.
	Landings []Landing `json:"landings"`
	// Conflicts is every result, in any project, that is stopped on a
	// merge conflict right now. Landings are recent history and are
	// capped; this is the standing list, so a conflict cannot fall off
	// the end of it by waiting.
	Conflicts []Conflict `json:"conflicts"`
	// Facts are the rest of what the ledger holds and a person may want
	// to see at a glance: capacity reservations, attestations, replicas,
	// disclosures awaiting the owner, side effects with an unknown
	// outcome, and grants.
	Facts     Facts          `json:"facts"`
	Projects  []Project      `json:"projects"`
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

// Conflict is a result waiting on a merge conflict: which project it
// wants into, which files disagreed, and what can be done about it.
type Conflict struct {
	Project  string `json:"project"`
	Artifact string `json:"artifact"`
	Landing  string `json:"landing"`
	// Node is the machine the project's canonical copy lives on, named so
	// a reader sees where the disagreement is rather than an id.
	Node  string   `json:"node,omitempty"`
	Files []string `json:"files,omitempty"`
	// Resolvable says git kept the half-merged tree, which is what both
	// an agent and a person need to work from.
	Resolvable bool `json:"resolvable,omitempty"`
	// Editable says the conflict can be resolved in the console. A sealed
	// project's data never leaves its home machine, so it cannot be.
	Editable bool `json:"editable,omitempty"`
	// Reason says in words why a result with no merge to work on stopped,
	// such as writing into a nested repository.
	Reason string `json:"reason,omitempty"`
	// Attempt is the task of a resolution already running, if one is.
	Attempt string    `json:"attempt,omitempty"`
	At      time.Time `json:"at"`
}

type Landing struct {
	ID       string    `json:"id"`
	Project  string    `json:"project"`
	Artifact string    `json:"artifact"`
	State    string    `json:"state"`
	Paths    int       `json:"paths"`
	Error    string    `json:"error,omitempty"`
	At       time.Time `json:"at"`
	// Files are the paths a conflict is on, so a reader sees which files
	// disagreed rather than only that something did.
	Files []string `json:"files,omitempty"`
	// Resolvable says an agent can be handed this conflict: git kept the
	// half-merged tree for it.
	Resolvable bool `json:"resolvable,omitempty"`
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
	// DisplayName is what people call this machine; Name stays the
	// identity that tasks, projects and agents refer to.
	DisplayName string `json:"display_name,omitempty"`
	Role        string `json:"role"`
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
	// ProtocolMismatch is set when the hub's last attempt to connect was
	// refused because the machine and the hub share no protocol version.
	ProtocolMismatch *ProtocolMismatch `json:"protocol_mismatch,omitempty"`
	Level            string            `json:"level,omitempty"`
	Region           string            `json:"region,omitempty"`
	// Snapshot is what the machine says it can do, entry by entry, with
	// the evidence and coverage behind each.
	Snapshot *ability.Snapshot `json:"snapshot,omitempty"`
	// Features are the optional capabilities the machine's advert lists;
	// nodewire.Advert.Features says which those are.
	Features []string `json:"features,omitempty"`
	// Health is the machine's room to work, as of its last advert.
	Health *nodewire.Health `json:"health,omitempty"`
	// ProjectsRoot is the directory this machine keeps projects under; a
	// project names a directory relative to it.
	ProjectsRoot string `json:"projects_root,omitempty"`
}

// ProtocolMismatch is the protocol version a machine speaks, Node, against
// the range HubMin–HubMax the hub speaks, and Upgrade, the side that has to
// be upgraded for the two to connect: UpgradeNode when the machine runs an
// older steve than the hub, UpgradeHub when it runs a newer one.
type ProtocolMismatch struct {
	Node    int    `json:"node"`
	HubMin  int    `json:"hub_min"`
	HubMax  int    `json:"hub_max"`
	Upgrade string `json:"upgrade"`
}

// The sides a protocol mismatch can ask to upgrade.
const (
	UpgradeNode = "node"
	UpgradeHub  = "hub"
)

type Harness struct {
	ID    string `json:"id"`
	Slots int    `json:"slots,omitempty"`
	// Version is the adapter as it introduced itself over ACP.
	Version string `json:"version,omitempty"`
	// Model is what the harness was last seen running here; Models what
	// it offers, declared or observed.
	Model  string   `json:"model,omitempty"`
	Models []string `json:"models,omitempty"`
	// Selectors are the other options the tool exposed here, such as
	// reasoning effort, so an agent can be given one when it is registered.
	Selectors []HarnessSelector `json:"selectors,omitempty"`
	Missing   string            `json:"missing,omitempty"`
}

// HarnessSelector is one option a tool offers on a machine, as last seen.
type HarnessSelector struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Category string   `json:"category,omitempty"`
	Current  string   `json:"current,omitempty"`
	Choices  []string `json:"choices,omitempty"`
	Values   []string `json:"values,omitempty"`
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
	// Reason is Why as a code a page can translate, with ReasonDetail as
	// its one free part (the error a machine reported, the model asked
	// for). Readers that cannot translate it still have Why.
	Reason       string   `json:"reason,omitempty"`
	ReasonDetail string   `json:"reason_detail,omitempty"`
	Requires     []string `json:"requires,omitempty"`
	// Preferred is the model the agent's configuration pins, applied at
	// session open; Observed is what the harness was last seen running.
	// Conditions are Requires judged against the machine, one by one.
	Preferred string `json:"preferred,omitempty"`
	Observed  string `json:"observed,omitempty"`
	// Options are the other selectors the agent pins; Selectors every
	// selector its harness exposed last time; About what it is for.
	Options map[string]string `json:"options,omitempty"`
	// Approval is the hub's default approval stance this agent follows
	// where it pins no mode of its own.
	Approval   string            `json:"approval,omitempty"`
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
	ID         string     `json:"id"`
	Goal       string     `json:"goal"`
	Title      string     `json:"title,omitempty"`
	Priority   string     `json:"priority,omitempty"`
	Labels     []string   `json:"labels,omitempty"`
	ArchivedAt string     `json:"archived_at,omitempty"`
	State      task.State `json:"state"`
	Member     string     `json:"member,omitempty"`
	NodeID     string     `json:"node,omitempty"`
	Parent     string     `json:"parent,omitempty"`
	// Children makes the tree explicit so a renderer does not have to build
	// it — the tree is the whole debugging story for delegated work.
	Children         []string  `json:"children,omitempty"`
	ChildrenCount    int       `json:"children_count"`
	ChildrenComplete bool      `json:"children_complete"`
	Turns            int       `json:"turns"`
	MaxTurns         int       `json:"max_turns"`
	Elapsed          string    `json:"elapsed"`
	MaxElapse        string    `json:"max_elapsed"`
	UpdatedAt        time.Time `json:"updated_at"`
	PlanID           string    `json:"plan_id,omitempty"`
	// Tokens and Seconds are what the task has spent across attempts.
	Tokens  Tokens `json:"tokens"`
	Seconds int64  `json:"seconds"`
	Model   string `json:"model,omitempty"`
	// Transport owns the opaque conversation in Channel; its prefix does
	// not identify the transport or authorize a control request.
	Transport string `json:"transport,omitempty"`
	// Channel, ProjectID and Origin say where the task was asked, in
	// which project, and by what (chat, plan, schedule, delegate).
	Channel   string `json:"channel,omitempty"`
	ProjectID string `json:"project_id,omitempty"`
	Origin    string `json:"origin,omitempty"`
	Requester string `json:"requester,omitempty"`
	// AttemptCount is how many turns the task has had; the rows themselves
	// are read page by page from its accounting (TaskDetail.Accounting).
	AttemptCount int `json:"attempt_count"`
	planInTree   bool
	// Four axes, decided here and rolled up from every descendant task:
	// Lifecycle is the task's own state; Execution says whether an
	// attempt is live (idle|running|unknown); Attention counts known requests
	// waiting; Lane is the board column that follows from the three.
	Lifecycle task.State `json:"lifecycle"`
	// Settlement is a person having closed a failed task by hand, either
	// because they dealt with it or because it does not matter. The task
	// keeps saying it failed; this says nobody is waiting on it any more.
	Settlement       task.Settlement `json:"settlement,omitempty"`
	Execution        ExecutionState  `json:"execution"`
	Attention        int             `json:"attention"`
	Lane             string          `json:"lane"`
	ResultDelivery   *task.Delivery  `json:"result_delivery,omitempty"`
	PendingResults   int             `json:"pending_results,omitempty"`
	UncertainResults int             `json:"uncertain_results,omitempty"`
	CanComplete      bool            `json:"can_complete,omitempty"`
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
	ID        string     `json:"id"`
	Goal      string     `json:"goal"`
	State     string     `json:"state"`
	Agent     string     `json:"agent,omitempty"`
	Node      string     `json:"node,omitempty"`
	Needs     []string   `json:"needs,omitempty"`
	Merge     []string   `json:"merge,omitempty"`
	Requires  []string   `json:"requires,omitempty"`
	Attempts  int        `json:"attempts,omitempty"`
	Verify    string     `json:"verify,omitempty"`
	Error     string     `json:"error,omitempty"`
	Usage     *StepUsage `json:"usage,omitempty"`
	StartedAt time.Time  `json:"started_at,omitzero"`
	EndedAt   time.Time  `json:"ended_at,omitzero"`
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
	// NodeNames maps node identities to display names when the hub is a
	// cluster member; nil when machines have no name besides their identity.
	NodeNames func() map[string]string
	Tasks     *task.Store
	Plans     PlanSource
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
	Observations ObservationStore
}

// ObservationStore keeps observations one record each, numbered from 1 in
// the order they were made, so recording one writes one record rather than
// the whole history. Observations (in ledger.go) keeps them in the ledger.
type ObservationStore interface {
	// Load returns every kept observation it can read, oldest first, and
	// the number of the last one kept, read or not; 0 when none is kept.
	// It fails only when the store cannot be read at all.
	Load(context.Context) ([]Observation, uint64, error)
	// Save writes list as the observations numbered first, first+1, …,
	// replacing any already kept under those numbers, and forgets every
	// observation numbered below keep: all of it in one write, or none.
	Save(ctx context.Context, first uint64, list []Observation, keep uint64) error
}

// LedgerSource is what the read model needs from the ledger-backed
// services: the live attempts and a project's landings.
type LedgerSource interface {
	LiveAttempts(ctx context.Context) ([]Attempt, error)
	RecentLandings(ctx context.Context) ([]Landing, error)
	// Conflicts is every project's standing merge conflict.
	Conflicts(ctx context.Context) ([]Conflict, error)
	Facts(ctx context.Context) (Facts, error)
	// ProjectList lists every project, for the page and the context bar.
	ProjectList(ctx context.Context) ([]project.Project, error)
	// UsageSamples are the spend fields of every attempt that reached a
	// terminal state: the authority on spend. HistoryEvents pages the journal
	// chronologically.
	UsageSamples(ctx context.Context) ([]attempt.UsageSample, error)
	HistoryEvents(ctx context.Context, before *ledger.EventPosition, through *int64, limit int) ([]ledger.Event, int64, error)
}

type NodeSource interface {
	Statuses() []node.Status
}

type PlanSource interface {
	Live() []plan.Plan
	ForTasks([]string) []plan.Plan
	Latest(string) (plan.Plan, bool)
	Count() int
	Query(plan.Query) (plan.Page, error)
	SetTaskProjection(func([]string))
}

// Model serves snapshots and a change stream.
type Model struct {
	interactions interface {
		Questions(string) []consoleapi.PendingQuestion
	}
	src Sources
	// taskHeader reads a task's header from src.Tasks; nil without one.
	taskHeader func(id string) (task.Header, bool)

	mu   sync.Mutex
	subs map[int]chan Event
	next int
	// epoch names this model's run of the stream and published counts its
	// events; together they are each event's stream id (see EventID).
	epoch     string
	published uint64
	// recent keeps the last events so a renderer that attaches mid-flight
	// has something to show immediately instead of a blank screen.
	recent       []Event
	throttle     map[string]throttled
	activity     map[string]Activity
	observations []Observation

	// observeMu orders observation updates and store I/O. mu protects the
	// in-memory list and event subscribers, never the slow I/O. Under
	// observeMu: loaded says the store's observations are in the list,
	// saved is the number of the last one the store keeps, and unsaved
	// counts the observations at the list's end it does not keep yet.
	observeMu sync.Mutex
	loaded    bool
	saved     uint64
	unsaved   int
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
	// Silent marks a console line the transcript does not draw, so a live
	// page hides it the moment it arrives rather than after its next read.
	Silent bool   `json:"silent,omitempty"`
	Title  string `json:"title,omitempty"`
	Rev    int    `json:"rev,omitempty"`
	Detail string `json:"detail,omitempty"`
	// Data carries an observation's facts apart from its sentence, so a
	// live page can say them the same way the history page does.
	Data map[string]string `json:"data,omitempty"`
	// Cursor is the event's place in the model's stream, in publication
	// order. The stream carries it as the event's id rather than in the
	// event itself.
	Cursor uint64 `json:"-"`
}

const recentKept = 200

func New(src Sources) *Model {
	if src.Plans != nil && src.Tasks != nil {
		src.Plans.SetTaskProjection(src.Tasks.SetPlanBindings)
	}
	m := &Model{src: src, subs: map[int]chan Event{}, epoch: strconv.FormatInt(time.Now().UnixNano(), 36)}
	if src.Tasks != nil {
		m.taskHeader = src.Tasks.Header
	}
	return m
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
	ByDay      []UsageRow             `json:"by_day"`
	ByAgent    []UsageRow             `json:"by_agent"`
	ByModel    []UsageRow             `json:"by_model"`
	Total      UsageRow               `json:"total"`
	Timezone   string                 `json:"timezone"`
	Periods    map[string]UsagePeriod `json:"periods"`
	ByHarness  []UsageRow             `json:"by_harness"`
	ByTrigger  []UsageRow             `json:"by_trigger"`
	ByProject  []UsageRow             `json:"by_project"`
	Tasks      TaskDurationStats      `json:"tasks"`
	ByTask     []TaskUsageRow         `json:"by_task"`
	Throughput Throughput             `json:"throughput"`
}

// UsagePeriod counts attempts by their start time in the hub's calendar.
// To is the snapshot time; the last bucket can be incomplete. Series keys
// are RFC3339 bucket starts with their local UTC offset, including during DST.
type UsagePeriod struct {
	From       time.Time         `json:"from"`
	To         time.Time         `json:"to"`
	Interval   string            `json:"interval"`
	Series     []UsageRow        `json:"series"`
	ByAgent    []UsageRow        `json:"by_agent"`
	ByModel    []UsageRow        `json:"by_model"`
	Total      UsageRow          `json:"total"`
	ByHarness  []UsageRow        `json:"by_harness"`
	ByTrigger  []UsageRow        `json:"by_trigger"`
	ByProject  []UsageRow        `json:"by_project"`
	Tasks      TaskDurationStats `json:"tasks"`
	ByTask     []TaskUsageRow    `json:"by_task"`
	Throughput Throughput        `json:"throughput"`
}

type UsageRow struct {
	Key      string `json:"key"`
	Tokens   Tokens `json:"tokens"`
	Seconds  int64  `json:"seconds"`
	Attempts int    `json:"attempts"`
	// Unreported counts attempts whose harness said nothing about tokens.
	Unreported int                `json:"unreported,omitempty"`
	Tasks      *TaskDurationStats `json:"tasks,omitempty"`
	TPM        float64            `json:"tpm,omitempty"`
}

// TaskDurationStats measures root task trees using the union of observed
// closed-execution intervals. Different root tasks remain separate samples.
type TaskDurationStats struct {
	Count          int     `json:"count"`
	Measured       int     `json:"measured"`
	MinSeconds     float64 `json:"min_seconds"`
	MaxSeconds     float64 `json:"max_seconds"`
	AverageSeconds float64 `json:"average_seconds"`
	TotalSeconds   float64 `json:"total_seconds"`
}

type TaskUsageRow struct {
	UsageRow
	TaskID         string  `json:"task_id"`
	Title          string  `json:"title"`
	Trigger        string  `json:"trigger"`
	Project        string  `json:"project"`
	Agent          string  `json:"agent"`
	Harness        string  `json:"harness"`
	Model          string  `json:"model"`
	ElapsedSeconds float64 `json:"elapsed_seconds"`
}

// Throughput uniformly allocates reported input/output over closed execution
// intervals. It estimates workload throughput, never token generation speed.
type Throughput struct {
	WindowTPM        float64 `json:"window_tpm"`
	ActiveTPM        float64 `json:"active_tpm"`
	PeakTPM          float64 `json:"peak_tpm"`
	Estimated        bool    `json:"estimated"`
	MeasuredTokens   float64 `json:"measured_tokens"`
	UnmeasuredTokens float64 `json:"unmeasured_tokens"`
	ActiveSeconds    float64 `json:"active_seconds"`
}

// HumanRequest is one thing only a person can settle, projected from the
// operation that is waiting: a disclosure, an effect with an unknown
// outcome. The operation stays the authority; this is how the inbox
// shows it, with the choices that are actually available.
type HumanRequest struct {
	Conversation string    `json:"conversation,omitempty"`
	AttemptID    string    `json:"attempt_id,omitempty"`
	Node         string    `json:"node,omitempty"`
	Workspace    string    `json:"workspace,omitempty"`
	ID           string    `json:"id"`
	Type         string    `json:"type"`
	Source       string    `json:"source"`
	ProjectID    string    `json:"project_id,omitempty"`
	TaskID       string    `json:"task_id,omitempty"`
	Summary      string    `json:"summary"`
	Choices      []Choice  `json:"choices"`
	CreatedAt    time.Time `json:"created_at"`
	Resolvable   bool      `json:"resolvable"`
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
	At        time.Time         `json:"at"`
	Seq       int64             `json:"seq,omitempty"`
	Kind      string            `json:"kind"`
	Subject   string            `json:"subject,omitempty"`
	Text      string            `json:"text"`
	Actor     string            `json:"actor,omitempty"`
	Operation string            `json:"operation,omitempty"`
	From      string            `json:"from,omitempty"`
	To        string            `json:"to,omitempty"`
	Data      map[string]string `json:"data,omitempty"`
}

// Observation is a connectivity fact worth remembering: a machine came
// up with a build, went down with a reason, a probe answered. Kept in the
// ledger, one record each, so history survives the process.
//
// Text is one English sentence, for logs and for readers of the raw
// record. Data carries the same facts apart, so a page can say them
// in the reader's language with the machine's own name rather than
// parsing the sentence back. The keys are per kind and documented at
// each call site; a reader that meets an unknown kind falls back to
// Text, which is why Text is never omitted.
type Observation struct {
	At      time.Time         `json:"at"`
	Kind    string            `json:"kind"`
	Subject string            `json:"subject"`
	Text    string            `json:"text"`
	Data    map[string]string `json:"data,omitempty"`
}

// Project is where work happens: a directory on one machine, with a data
// level and a repo mode, and the agents that could work it right now —
// judged by the same rule a turn is judged by.
type Project struct {
	TaskCounts  *task.Counts `json:"task_counts"`
	ID          string       `json:"id"`
	Node        string       `json:"node"`
	Path        string       `json:"path"`
	Level       string       `json:"level"`
	Repo        string       `json:"repo"`
	DefaultRole string       `json:"default_role,omitempty"`
	Agents      []string     `json:"agents"`
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
			ID: t.ID, Kind: t.Kind, Name: t.Name, Detail: t.Detail, Status: t.Status,
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
// parent's delegate call. Only a running child is throttled — the state
// it ends in, finished or stopped, is its last word and must land.
func (m *Model) DelegateProgress(childTaskID, agent, node string, info consoleapi.StepInfo, p view.Progress) {
	stepID := "#" + childTaskID
	key := "delegate/" + stepID
	terminal := info.State != task.StateRunning
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
// tool call starting or finishing always is. An update the throttle holds
// back is published when the step's window ends, unless a later one was
// published first, so a step's last update is never the one dropped.
// What that guarantees is that the update appears in the model's event
// stream, up to a window late; a subscriber that stopped before then,
// such as a turn's reply that ended within the window, does not receive
// it. StepEnded publishes the snapshot a step's prompt returned with at
// once. A step's updates are expected one call at a time, in the order
// its session reports them, and are published in that order; calls for
// one step that overlap have no order the model can keep.
func (m *Model) StepProgress(taskID, planID, stepID, agent, node string, p view.Progress) {
	key := planID + "/" + stepID
	signature := fmt.Sprintf("%d", len(p.Tools))
	if n := len(p.Tools); n > 0 {
		signature += "/" + string(p.Tools[n-1].Status)
	}
	ev := stepEvent(taskID, planID, stepID, agent, node, p)
	// An update that only replaces one already held takes the held one's
	// conversation: the task store is read, outside the lock, for updates
	// that publish or start a hold, not for each one merged.
	m.mu.Lock()
	last := m.throttle[key]
	if last.held != nil && last.held.TaskID == taskID && last.signature == signature && time.Since(last.at) < progressEvery {
		ev.Conversation = last.held.Conversation
		last.held = &ev
		m.throttle[key] = last
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()
	ev.Conversation = m.conversationOf(taskID)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.throttle == nil {
		m.throttle = map[string]throttled{}
	}
	last = m.throttle[key]
	if wait := progressEvery - time.Since(last.at); wait > 0 && last.signature == signature {
		if last.held == nil {
			time.AfterFunc(wait, func() { m.releaseStep(key) })
		}
		last.held = &ev
		m.throttle[key] = last
		return
	}
	now := time.Now()
	m.sweepThrottleLocked(now)
	m.throttle[key] = throttled{at: now, signature: signature}
	m.publishLocked(ev)
}

// StepEnded publishes the snapshot a step's prompt returned with, without
// throttling. An update the throttle holds for the step is dropped rather
// than published after it. It marks the end of that prompt, not always
// of the step: a node-owned step whose observer was lost keeps running on
// its node, and once resumed it reports progress again, in a window of
// its own, and ends with another StepEnded when the resumed prompt
// returns.
func (m *Model) StepEnded(taskID, planID, stepID, agent, node string, p view.Progress) {
	ev := stepEvent(taskID, planID, stepID, agent, node, p)
	ev.Conversation = m.conversationOf(taskID)
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.throttle, planID+"/"+stepID)
	m.publishLocked(ev)
}

// stepEvent is a step's update as the model publishes it, without its
// conversation.
func stepEvent(taskID, planID, stepID, agent, node string, p view.Progress) Event {
	progress := FromProgress(p)
	if progress.Agent == "" {
		progress.Agent = agent
	}
	if progress.Node == "" {
		progress.Node = node
	}
	return Event{Kind: "step.progress", TaskID: taskID, PlanID: planID, StepID: stepID, Progress: &progress}
}

// sweepThrottleLocked forgets keys whose window ended with nothing held.
func (m *Model) sweepThrottleLocked(now time.Time) {
	for key, last := range m.throttle {
		if last.held == nil && now.Sub(last.at) >= progressEvery {
			delete(m.throttle, key)
		}
	}
}

// releaseStep publishes the update a step's throttle held back, once the
// window it was held in has ended. Publishing under the same lock as
// StepProgress keeps it from landing after a later update.
func (m *Model) releaseStep(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	last := m.throttle[key]
	if last.held == nil || time.Since(last.at) < progressEvery {
		// A later update was published first; a window it opened has its
		// own release.
		return
	}
	m.throttle[key] = throttled{at: time.Now(), signature: last.signature}
	ev := *last.held
	agent := ev.Progress.Agent
	current, moved := m.activity[agent]
	moved = moved && (current.TaskID != ev.TaskID || current.StepID != ev.StepID)
	m.publishLocked(ev)
	if moved {
		// The agent went on to another step while this update was held:
		// its activity stays on that step.
		m.activity[agent] = current
	}
}

type throttled struct {
	at        time.Time
	signature string
	// held is a step's latest update inside its window, still to publish.
	held *Event
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
	if taskID == "" || m.taskHeader == nil {
		return ""
	}
	if t, ok := m.taskHeader(taskID); ok {
		return t.Channel
	}
	return ""
}

// taskOfPlan is the task a plan belongs to, through the plan store.
func (m *Model) taskOfPlan(planID string) string {
	if planID == "" || m.src.Plans == nil {
		return ""
	}
	if p, ok := m.src.Plans.Latest(planID); ok {
		return p.TaskID
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
	m.mu.Lock()
	defer m.mu.Unlock()
	m.publishLocked(ev)
}

// publishLocked is Publish with m.mu held.
func (m *Model) publishLocked(ev Event) {
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	m.published++
	ev.Cursor = m.published
	m.noteActivity(ev)
	m.recent = append(m.recent, ev)
	if len(m.recent) > recentKept {
		m.recent = m.recent[len(m.recent)-recentKept:]
	}
	// Sent under the lock, so a subscriber cancelling — which closes its
	// channel — cannot race a send. The send never blocks: a subscriber
	// that cannot keep up is closed instead of handed a gap, so an open
	// stream has missed nothing and a closed one knows to re-read the
	// snapshot when it subscribes again.
	for id, ch := range m.subs {
		select {
		case ch <- ev:
		default:
			delete(m.subs, id)
			close(ch)
			slog.Warn("readmodel: closed a subscriber that fell behind", "subscriber", id, "event", ev.Kind)
		}
	}
}

// Observe records a connectivity fact and tells the page. The store keeps
// each observation as its own record, so a restart does not forget it and
// recording one writes only what the store does not have yet. A failed save
// leaves the fact live; later Observes write it with their own, oldest
// first and at most observationsPerSave at a time.
func (m *Model) Observe(kind, subject, text string, data map[string]string) {
	obs := Observation{At: time.Now().UTC(), Kind: kind, Subject: subject, Text: text, Data: data}
	m.observeMu.Lock()
	defer m.observeMu.Unlock()
	store := m.src.Observations
	if store != nil && !m.loaded {
		// Numbering new records before the kept ones are known would
		// file them among the old; they wait in memory until a load works.
		if err := m.loadObservations(); err != nil {
			slog.Error(fmt.Sprintf("readmodel: load observations: %v", err))
		}
	}
	m.mu.Lock()
	m.observations = append(m.observations, obs)
	m.unsaved++
	m.keepObservations()
	unsaved := m.observations[len(m.observations)-m.unsaved:]
	pending := append([]Observation(nil), unsaved[:min(len(unsaved), observationsPerSave)]...)
	m.mu.Unlock()
	if store != nil && m.loaded {
		first := m.saved + 1
		last := m.saved + uint64(len(pending))
		var keep uint64
		if last > observationsKept {
			keep = last - observationsKept + 1
		}
		if err := store.Save(context.Background(), first, pending, keep); err != nil {
			slog.Error(fmt.Sprintf("readmodel: save observations: %v", err))
		} else {
			m.saved = last
			m.unsaved -= len(pending)
		}
	}
	m.Publish(Event{Kind: "observe." + kind, Detail: text, Text: subject, Data: data})
}

const observationsKept = 1000

// observationsPerSave bounds one save, which the ledger replicates as one
// write: an observation record is under 1 KB, so a save stays near 30 KB
// however long saving failed, instead of resending the backlog, up to the
// whole retained history, in one write to members that may sit behind slow
// links. Each save still drains observationsPerSave-1 more than the one
// observed, so a full backlog is caught up within a few dozen observations.
const observationsPerSave = 32

// keepObservations forgets the oldest observations past the limit, saved
// or not. The caller holds observeMu and mu.
func (m *Model) keepObservations() {
	if len(m.observations) > observationsKept {
		m.observations = m.observations[len(m.observations)-observationsKept:]
	}
	m.unsaved = min(m.unsaved, len(m.observations))
}

// LoadObservations brings back what an earlier process observed. What this
// process observed and has not saved yet stays, after them, once each.
func (m *Model) LoadObservations() error {
	m.observeMu.Lock()
	defer m.observeMu.Unlock()
	if m.src.Observations == nil {
		return nil
	}
	return m.loadObservations()
}

// loadObservations runs under observeMu.
func (m *Model) loadObservations() error {
	list, last, err := m.src.Observations.Load(context.Background())
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.loaded && last > m.saved {
		// Once loaded, only this model writes, oldest unsaved first: numbers
		// past the last it knows saved are saves that committed although
		// they were reported as failed. Those observations are in list.
		m.unsaved -= int(min(last-m.saved, uint64(m.unsaved)))
	}
	m.observations = append(list, m.observations[len(m.observations)-m.unsaved:]...)
	m.keepObservations()
	m.saved, m.loaded = last, true
	return nil
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
		if t.Status == view.ToolRunning {
			break
		}
	}
	m.activity[ev.Progress.Agent] = next
}

// Subscribe returns a channel of changes and a cancel function. The
// channel is closed when ctx ends, when cancel is called, or when the
// subscriber falls behind and an event could not be delivered to it.
func (m *Model) Subscribe(ctx context.Context) (<-chan Event, func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.subscribeLocked(ctx)
}

func (m *Model) subscribeLocked(ctx context.Context) (<-chan Event, func()) {
	ch := make(chan Event, 64)
	id := m.next
	m.next++
	m.subs[id] = ch
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
