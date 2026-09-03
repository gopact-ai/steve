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
	"github.com/gopact-ai/steve/internal/models"
	"github.com/gopact-ai/steve/internal/nodewire"
	"sync"
	"time"

	"github.com/gopact-ai/gopact"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/roster"
	"github.com/gopact-ai/steve/internal/task"
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
	Facts Facts `json:"facts"`
}

// Facts is the ledger seen from the outside.
type Facts struct {
	Reservations []Reservation `json:"reservations"`
	Attestations []Attestation `json:"attestations"`
	Replicas     []Replica     `json:"replicas"`
	Disclosures  []Disclosure  `json:"disclosures"`
	Effects      []Effect      `json:"effects"`
	Grants       []Grant       `json:"grants"`
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
	Level  string          `json:"level,omitempty"`
	Advert nodewire.Advert `json:"advert"`
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
}

type Harness struct {
	ID    string `json:"id"`
	Slots int    `json:"slots,omitempty"`
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
	Level    string   `json:"level,omitempty"`
	Slots    int      `json:"slots,omitempty"`
	Region   string   `json:"region,omitempty"`
}

type Task struct {
	ID     string `json:"id"`
	Goal   string `json:"goal"`
	State  string `json:"state"`
	Member string `json:"member,omitempty"`
	NodeID string `json:"node,omitempty"`
	Parent string `json:"parent,omitempty"`
	// Children makes the tree explicit so a renderer does not have to build
	// it — the tree is the whole debugging story for delegated work.
	Children  []string  `json:"children,omitempty"`
	Turns     int       `json:"turns"`
	MaxTurns  int       `json:"max_turns"`
	Elapsed   string    `json:"elapsed"`
	MaxElapse string    `json:"max_elapsed"`
	UpdatedAt time.Time `json:"updated_at"`
	PlanID    string    `json:"plan_id,omitempty"`
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
	Context *StepContext `json:"context,omitempty"`
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
	// Models is the book of observed models, for node harnesses that
	// declare none.
	Models Models
}

// LedgerSource is what the read model needs from the ledger-backed
// services: the live attempts and a project's landings.
type LedgerSource interface {
	LiveAttempts(ctx context.Context) []Attempt
	RecentLandings(ctx context.Context) []Landing
	Facts(ctx context.Context) Facts
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
	recent []Event
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
	// from the page, what came back, and milestones agents posted.
	Conversation string `json:"conversation,omitempty"`
	Text         string `json:"text,omitempty"`
	Title        string `json:"title,omitempty"`
	Rev          int    `json:"rev,omitempty"`
	Detail       string `json:"detail,omitempty"`
}

const recentKept = 200

func New(src Sources) *Model {
	return &Model{src: src, subs: map[int]chan Event{}}
}

// Emit makes the model a gopact.EventSink, so workflow node transitions reach
// the renderers as they happen rather than on the next poll.
//
// gopact's own run log remains the durable authority; this is the live
// projection for screens. A renderer that misses an event re-reads the
// snapshot, which is why dropping is preferable to blocking the runtime.
func (m *Model) Emit(_ context.Context, ev gopact.Event) error {
	m.Publish(Event{
		At:     ev.Timestamp,
		Kind:   ev.Type,
		RunID:  ev.RunID,
		PlanID: ev.DefinitionID,
		StepID: ev.NodeID,
		Seq:    ev.Sequence,
		Detail: ev.Summary,
	})
	return nil
}

func (m *Model) Publish(ev Event) {
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	m.mu.Lock()
	m.recent = append(m.recent, ev)
	if len(m.recent) > recentKept {
		m.recent = m.recent[len(m.recent)-recentKept:]
	}
	subs := make([]chan Event, 0, len(m.subs))
	for _, ch := range m.subs {
		subs = append(subs, ch)
	}
	m.mu.Unlock()
	for _, ch := range subs {
		// Never block the publisher: a renderer that cannot keep up misses
		// events and re-reads the snapshot, which is the right tradeoff.
		select {
		case ch <- ev:
		default:
		}
	}
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
