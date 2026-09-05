// Package attempt is the one execution Operation in Steve.
//
// A Turn is a logical request — a chat message, a plan step, a delegation.
// An Attempt is one execution of it, recorded in the ledger as an Operation
// with the state machine below. Every transition names the leases it was
// checked against, so an attempt that lost its lease cannot move: not to
// bound, not to failed, not anywhere.
//
//	leased → prepared → running → snapshotted → published → durable
//	       → [verifying] → bind-ready → bound
//	any non-terminal → failed | expired | superseded
//	bind-ready → bind-conflict
//
// A running attempt with no artifact to publish — a read-only turn, or one
// whose output is only text — goes from running to bind-ready directly;
// the events say so.
package attempt

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/ability"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
)

type State string

const (
	Leased       State = "leased"
	Prepared     State = "prepared"
	Running      State = "running"
	Snapshotted  State = "snapshotted"
	Published    State = "published"
	Durable      State = "durable"
	Verifying    State = "verifying"
	BindReady    State = "bind-ready"
	Bound        State = "bound"
	Failed       State = "failed"
	Expired      State = "expired"
	BindConflict State = "bind-conflict"
	Superseded   State = "superseded"
)

var interrupts = []State{Failed, Expired, Superseded}

var transitions = map[State][]State{
	Leased:      append([]State{Prepared}, interrupts...),
	Prepared:    append([]State{Running}, interrupts...),
	Running:     append([]State{Snapshotted, BindReady}, interrupts...),
	Snapshotted: append([]State{Published}, interrupts...),
	Published:   append([]State{Durable}, interrupts...),
	Durable:     append([]State{Verifying, BindReady}, interrupts...),
	Verifying:   append([]State{BindReady}, interrupts...),
	BindReady:   append([]State{Bound, BindConflict}, interrupts...),
}

// Terminal says the attempt is over.
func (s State) Terminal() bool { return len(transitions[s]) == 0 }

func (s State) can(to State) bool {
	for _, next := range transitions[s] {
		if next == to {
			return true
		}
	}
	return false
}

// Kind is what kind of turn the attempt executes.
type Kind string

const (
	KindChat     Kind = "chat"
	KindStep     Kind = "step"
	KindDelegate Kind = "delegate"
	KindVerify   Kind = "verify"
	KindPlan     Kind = "plan"
)

// Scope is the WriteScope fixed for the attempt.
type Scope string

const (
	// ScopeUnrestricted is an in-place turn on the canonical workspace,
	// under the canonical write lock — or on a copy, under that copy's.
	ScopeUnrestricted Scope = "unrestricted"
	// ScopePathSet is a step in an isolated workspace, allowed to touch
	// only the declared paths.
	ScopePathSet Scope = "path-set"
	// ScopeNone is read-only.
	ScopeNone Scope = "none"
)

// Spec is what an attempt is fixed to when it opens.
type Spec struct {
	ID      string `json:"id"`
	TaskID  string `json:"task_id"`
	TurnID  string `json:"turn_id"`
	Kind    Kind   `json:"kind"`
	Project string `json:"project"`
	Node    string `json:"node"`
	Harness string `json:"harness"`
	Agent   string `json:"agent"`
	// Slots is the endpoint's capacity for (node, harness); zero is
	// unlimited and takes no slot lease.
	Slots int `json:"slots,omitempty"`
	// Region is the region of the machine the attempt runs on: its slot
	// and workspace leases are issued there. CanonicalRegion is where the
	// project's canonical workspace lives, for the write lock. Empty is
	// the hub's own region.
	Region          string `json:"region,omitempty"`
	CanonicalRegion string `json:"canonical_region,omitempty"`
	// Reservation names a capacity reservation whose slot this attempt
	// takes over instead of competing for one.
	Reservation string            `json:"reservation,omitempty"`
	Workspace   project.Workspace `json:"workspace"`
	Scope       Scope             `json:"scope"`
	Touches     []string          `json:"touches,omitempty"`
	Base        string            `json:"base,omitempty"`
	By          string            `json:"by,omitempty"`
	// Requires is what the work asked of the machine, as placed.
	Requires []string `json:"requires,omitempty"`
}

// Result is what a finished attempt produced.
type Result struct {
	Summary  string   `json:"summary,omitempty"`
	Artifact string   `json:"artifact,omitempty"`
	Refs     []string `json:"refs,omitempty"`
}

// Usage is an attempt's spend. Reported false means the harness said
// nothing about tokens — not the same as zero.
type Usage struct {
	Model       string `json:"model,omitempty"`
	Input       int64  `json:"input,omitempty"`
	Output      int64  `json:"output,omitempty"`
	CachedRead  int64  `json:"cached_read,omitempty"`
	CachedWrite int64  `json:"cached_write,omitempty"`
	// Context is the context window in use at the last report — what
	// ACP adapters reliably say, when they say nothing about tokens.
	Context  int64 `json:"context,omitempty"`
	Reported bool  `json:"reported"`
}

// Record is the attempt as the ledger holds it.
type Record struct {
	Spec
	State    State          `json:"state"`
	Revision int64          `json:"revision"`
	Session  string         `json:"session,omitempty"`
	Leases   []ledger.Lease `json:"leases"`
	Result   *Result        `json:"result,omitempty"`
	// Usage is what the attempt cost, as the harness last reported it,
	// written with every terminal transition — success, failure, expiry
	// alike — so failed work is not free in the books.
	Usage *Usage `json:"usage,omitempty"`
	// Admission is the final check made before the attempt ran: who
	// judged Requires, on which snapshot, with what result. Written at
	// leased→prepared.
	Admission    *ability.Admission `json:"admission,omitempty"`
	Error        string             `json:"error,omitempty"`
	SupersededBy string             `json:"superseded_by,omitempty"`
	StartedAt    time.Time          `json:"started_at"`
	EndedAt      time.Time          `json:"ended_at,omitempty"`
}

// Busy is a refused open: a resource the attempt needs is leased to
// another holder.
type Busy struct {
	Resource string
	Holder   string
	Until    time.Time
}

func (b Busy) Error() string {
	return fmt.Sprintf("attempt: %s is held by %s until %s", b.Resource, b.Holder, b.Until.Format(time.RFC3339))
}

// NoSlot is a refused open: every endpoint slot is taken.
type NoSlot struct {
	Endpoint string
	Slots    int
}

func (n NoSlot) Error() string {
	return fmt.Sprintf("attempt: all %d slots of %s are taken", n.Slots, n.Endpoint)
}

var (
	ErrLost     = errors.New("attempt: lease lost")
	ErrBadState = errors.New("attempt: transition not allowed")
)

const (
	kind       = "attempt"
	DefaultTTL = 90 * time.Second
)

// Service opens, moves and sweeps attempts.
type Service struct {
	l   *ledger.Ledger
	now func() time.Time
	// TTL is how long a lease lives without renewal.
	TTL time.Duration
}

func New(l *ledger.Ledger) *Service {
	return &Service{l: l, now: time.Now, TTL: DefaultTTL}
}

// NewID mints an attempt id ahead of Open, for a workspace that has to be
// named after its attempt before the attempt can be leased on it.
func NewID() string { return newID() }

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "att-" + hex.EncodeToString(b[:])
}

func endpointKey(node, harness string) string {
	if node == "" {
		node = "hub"
	}
	return "endpoint:" + node + "/" + harness
}

// Hold takes a resource's lease for the caller — an admin action that
// must not race a turn, such as forgetting a copy — and returns how to let
// go. A resource someone holds is reported as Busy.
func (s *Service) Hold(ctx context.Context, region, key, holder string) (func(), error) {
	lease, err := s.l.AcquireIn(ctx, region, key, holder, s.TTL)
	if err != nil {
		if errors.Is(err, ledger.ErrHeld) {
			current, ok, _ := s.l.LeaseOf(ctx, key)
			busy := Busy{Resource: key}
			if ok {
				busy.Holder, busy.Until = current.Holder, current.ExpiresAt
			}
			return nil, busy
		}
		return nil, err
	}
	return func() { _ = s.l.ReleaseAny(context.Background(), lease) }, nil
}

// Open takes every lease the attempt needs and records it leased. It is
// all or nothing: a lease that cannot be had releases the ones already
// taken and reports which resource is busy.
func (s *Service) Open(ctx context.Context, spec Spec) (Record, error) {
	if spec.ID == "" {
		spec.ID = newID()
	}
	if spec.Scope == "" {
		return Record{}, errors.New("attempt: write scope must be fixed")
	}
	if spec.Workspace.Project != "" && spec.Workspace.Project != spec.Project {
		return Record{}, fmt.Errorf("attempt: workspace %s belongs to project %s, not %s", spec.Workspace.ID, spec.Workspace.Project, spec.Project)
	}
	inPlace := spec.Workspace.Kind == project.KindCanonical || spec.Workspace.Kind == project.KindCopy
	if inPlace && spec.Workspace.Node != spec.Node {
		// The lock is issued where the workspace is; an attempt elsewhere
		// would lock one directory and write another.
		return Record{}, fmt.Errorf("attempt: workspace %s is on %q, the attempt on %q", spec.Workspace.ID, spec.Workspace.Node, spec.Node)
	}
	if spec.Scope != ScopeUnrestricted && inPlace {
		return Record{}, fmt.Errorf("attempt: scope %s is only allowed in an isolated workspace", spec.Scope)
	}
	if spec.Scope == ScopeUnrestricted && !inPlace {
		return Record{}, errors.New("attempt: unrestricted scope is only allowed in place")
	}
	var held []ledger.Lease
	release := func() {
		for _, lease := range held {
			_ = s.l.ReleaseAny(ctx, lease)
		}
	}
	take := func(region, key string) error {
		lease, err := s.l.AcquireIn(ctx, region, key, spec.ID, s.TTL)
		if err != nil {
			if errors.Is(err, ledger.ErrHeld) {
				holder, until := "", time.Time{}
				if current, ok, _ := s.l.LeaseOf(ctx, key); ok && (region == "" || region == s.l.Region()) {
					holder, until = current.Holder, current.ExpiresAt
				}
				return Busy{Resource: key, Holder: holder, Until: until}
			}
			return err
		}
		held = append(held, lease)
		return nil
	}
	if err := take("", "attempt:"+spec.ID); err != nil {
		return Record{}, err
	}
	switch spec.Workspace.Kind {
	case project.KindCanonical:
		if spec.Scope == ScopeUnrestricted {
			if err := take(spec.CanonicalRegion, "canonical:"+spec.Project); err != nil {
				release()
				return Record{}, err
			}
		}
	case project.KindCopy:
		// A copy is one directory with one writer; its lock is its own
		// id ("copy:<project>/<node>"), issued where the copy is.
		if err := take(spec.Region, spec.Workspace.ID); err != nil {
			release()
			return Record{}, err
		}
	case project.KindWorktree:
		if err := take(spec.Region, "workspace:"+spec.Workspace.ID); err != nil {
			release()
			return Record{}, err
		}
	}
	if spec.Reservation != "" {
		// The reservation's slot becomes this attempt's, atomically: an
		// attempt that arrives late finds its slot waiting, not taken.
		taken, err := s.takeReservation(ctx, spec, held)
		if err != nil {
			release()
			return Record{}, err
		}
		held = taken
	} else if spec.Slots > 0 {
		endpoint := endpointKey(spec.Node, spec.Harness)
		taken := false
		for i := 1; i <= spec.Slots; i++ {
			err := take(spec.Region, fmt.Sprintf("%s:slot:%d", endpoint, i))
			if err == nil {
				taken = true
				break
			}
			var busy Busy
			if !errors.As(err, &busy) {
				release()
				return Record{}, err
			}
		}
		if !taken {
			release()
			return Record{}, NoSlot{Endpoint: endpoint, Slots: spec.Slots}
		}
	}
	record := Record{Spec: spec, State: Leased, Revision: 1, Leases: held, StartedAt: s.now().UTC()}
	if _, err := s.l.Begin(ctx, spec.ID, kind, string(Leased), spec.By, record); err != nil {
		release()
		return Record{}, err
	}
	return record, nil
}

// Advance moves an attempt. Every lease it holds is a fencing on the
// transition; a terminal state releases them afterwards.
func (s *Service) Advance(ctx context.Context, id string, to State, actor string, mutate func(*Record)) (Record, error) {
	current, err := s.Get(ctx, id)
	if err != nil {
		return Record{}, err
	}
	if !current.State.can(to) {
		return Record{}, fmt.Errorf("%w: %s → %s", ErrBadState, current.State, to)
	}
	var next Record
	_, err = s.l.Transition(ctx, id, string(current.State), string(to), actor, current.Leases, nil,
		func(tx *ledger.Tx, op *ledger.Operation) error {
			next = current
			if err := json.Unmarshal(op.Data, &next); err != nil {
				return err
			}
			next.State = to
			next.Revision = op.Revision + 1
			if mutate != nil {
				mutate(&next)
			}
			if to.Terminal() {
				next.EndedAt = s.now().UTC()
			}
			return tx.SetData(op, next)
		})
	if err != nil {
		if errors.Is(err, ledger.ErrStale) {
			return Record{}, fmt.Errorf("%w: %v", ErrLost, err)
		}
		return Record{}, err
	}
	if to.Terminal() {
		for _, lease := range next.Leases {
			_ = s.l.ReleaseAny(ctx, lease)
		}
	}
	return next, nil
}

// Renew extends every lease. A lease that no longer matches means the
// attempt has been superseded or expired underneath: the caller must stop.
func (s *Service) Renew(ctx context.Context, id string) error {
	current, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if current.State.Terminal() {
		return fmt.Errorf("%w: attempt is %s", ErrLost, current.State)
	}
	renewed := make([]ledger.Lease, 0, len(current.Leases))
	for _, lease := range current.Leases {
		next, err := s.l.RenewAny(ctx, lease, s.TTL)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrLost, err)
		}
		renewed = append(renewed, next)
	}
	// Expiry times moved; the record keeps the tuple, which is what
	// fencing compares, so nothing needs writing back.
	return nil
}

// Heartbeat renews until ctx ends. lost is closed the first time a renewal
// fails; the driver aborts the work then, because nothing it does after can
// be recorded.
func (s *Service) Heartbeat(ctx context.Context, id string) (lost <-chan struct{}) {
	ch := make(chan struct{})
	go func() {
		interval := s.TTL / 3
		if interval <= 0 {
			interval = 10 * time.Second
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := s.Renew(ctx, id); err != nil {
					if ctx.Err() == nil {
						log.Printf("attempt %s: heartbeat: %v", id, err)
					}
					close(ch)
					return
				}
			}
		}
	}()
	return ch
}

// Finish records success. An attempt with an artifact is expected to have
// walked the publish states already; one without goes bind-ready now.
func (s *Service) Finish(ctx context.Context, id, actor string, result Result) (Record, error) {
	return s.FinishWith(ctx, id, actor, result, nil)
}

// FinishWith is Finish with what the attempt cost, written in the same
// transition as its result: one record, one place the spend is true.
func (s *Service) FinishWith(ctx context.Context, id, actor string, result Result, usage *Usage) (Record, error) {
	current, err := s.Get(ctx, id)
	if err != nil {
		return Record{}, err
	}
	meter := func(r *Record) {
		if usage != nil {
			r.Usage = usage
		}
	}
	if current.State == Running || current.State == Durable || current.State == Verifying {
		if _, err := s.Advance(ctx, id, BindReady, actor, func(r *Record) { r.Result = &result; meter(r) }); err != nil {
			return Record{}, err
		}
	}
	return s.Advance(ctx, id, Bound, actor, func(r *Record) {
		if r.Result == nil {
			r.Result = &result
		}
		meter(r)
	})
}

// Fail records a failure from any live state.
func (s *Service) Fail(ctx context.Context, id, actor, cause string) (Record, error) {
	return s.FailWith(ctx, id, actor, cause, nil)
}

// FailWith is Fail with what the attempt cost anyway: failed work is not
// free, and the books say so.
func (s *Service) FailWith(ctx context.Context, id, actor, cause string, usage *Usage) (Record, error) {
	return s.Advance(ctx, id, Failed, actor, func(r *Record) {
		r.Error = cause
		if usage != nil {
			r.Usage = usage
		}
	})
}

// Supersede is the takeover: old must be over in a way that leaves work
// to redo and must not have been in place; its leases are invalidated —
// only its own, a resource someone else has since taken is untouched —
// and the new attempt opens.
func (s *Service) Supersede(ctx context.Context, oldID string, spec Spec, actor string) (Record, error) {
	old, err := s.Get(ctx, oldID)
	if err != nil {
		return Record{}, err
	}
	if old.Scope == ScopeUnrestricted {
		return Record{}, fmt.Errorf("attempt %s ran in place; it cannot be taken over, only retried by its turn", oldID)
	}
	from := old.State
	switch old.State {
	case Expired, Failed, BindConflict:
	case Leased, Prepared, Running, Snapshotted, Published, Durable, Verifying, BindReady:
		// A live attempt is expired first, unfenced: its own leases may be
		// dead already, and the point is to cut them so that even if the
		// process behind it is still alive, nothing it writes lands.
		if err := s.expire(ctx, old, actor, "superseded while live"); err != nil {
			return Record{}, err
		}
		from = Expired
	default:
		return Record{}, fmt.Errorf("attempt %s is %s; nothing to take over", oldID, old.State)
	}
	if err := s.l.Invalidate(ctx, "attempt:"+oldID); err != nil {
		return Record{}, err
	}
	if _, err := s.l.InvalidateHeldBy(ctx, oldID); err != nil {
		return Record{}, err
	}
	for _, lease := range old.Leases {
		if lease.Region != "" && lease.Region != s.l.Region() {
			_ = s.l.InvalidateIn(ctx, lease.Region, lease.Key)
		}
	}
	if spec.ID == "" {
		spec.ID = newID()
	}
	if spec.Base == "" {
		spec.Base = old.Base
	}
	if _, err := s.l.Transition(ctx, oldID, string(from), string(Superseded), actor, nil, map[string]string{"by": spec.ID},
		func(tx *ledger.Tx, op *ledger.Operation) error {
			var r Record
			if err := json.Unmarshal(op.Data, &r); err != nil {
				return err
			}
			r.State = Superseded
			r.SupersededBy = spec.ID
			r.Revision = op.Revision + 1
			return tx.SetData(op, r)
		}); err != nil {
		return Record{}, err
	}
	return s.Open(ctx, spec)
}

// Sweep expires every live attempt whose own lease has run out — a hub
// that died mid-turn, a driver that stopped renewing — and cuts the leases
// it still held.
func (s *Service) Sweep(ctx context.Context) ([]Record, error) {
	live, err := s.Live(ctx)
	if err != nil {
		return nil, err
	}
	var expired []Record
	for _, r := range live {
		lease, ok, err := s.l.LeaseOf(ctx, "attempt:"+r.ID)
		if err != nil {
			return expired, err
		}
		if ok && lease.Holder == r.ID && lease.ExpiresAt.After(s.now()) {
			continue
		}
		if err := s.expire(ctx, r, "sweeper", "lease expired"); err != nil {
			continue // it moved on its own between the list and now
		}
		_, _ = s.l.InvalidateHeldBy(ctx, r.ID)
		r.State = Expired
		expired = append(expired, r)
	}
	return expired, nil
}

// expire is the unfenced edge into expired: the attempt's leases are, by
// definition, not something it can prove any more.
func (s *Service) expire(ctx context.Context, r Record, actor, cause string) error {
	_, err := s.l.Transition(ctx, r.ID, string(r.State), string(Expired), actor, nil, nil,
		func(tx *ledger.Tx, op *ledger.Operation) error {
			var next Record
			if err := json.Unmarshal(op.Data, &next); err != nil {
				return err
			}
			next.State = Expired
			next.Error = cause
			next.Revision = op.Revision + 1
			next.EndedAt = s.now().UTC()
			return tx.SetData(op, next)
		})
	return err
}

// Get reads one attempt.
func (s *Service) Get(ctx context.Context, id string) (Record, error) {
	op, ok, err := s.l.Operation(ctx, id)
	if err != nil {
		return Record{}, err
	}
	if !ok {
		return Record{}, fmt.Errorf("attempt %s not found", id)
	}
	return decode(op)
}

// Live lists every attempt that is not over.
func (s *Service) Live(ctx context.Context) ([]Record, error) {
	ops, err := s.l.Operations(ctx, kind, "")
	if err != nil {
		return nil, err
	}
	var out []Record
	for _, op := range ops {
		if State(op.State).Terminal() {
			continue
		}
		r, err := decode(op)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// Closed lists every attempt that reached a terminal state, oldest first:
// the fleet's spend is the sum of their results.
func (s *Service) Closed(ctx context.Context) ([]Record, error) {
	ops, err := s.l.Operations(ctx, kind, "")
	if err != nil {
		return nil, err
	}
	var out []Record
	for _, op := range ops {
		if !State(op.State).Terminal() {
			continue
		}
		r, err := decode(op)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// ForTask lists a task's attempts, oldest first.
func (s *Service) ForTask(ctx context.Context, taskID string) ([]Record, error) {
	ops, err := s.l.Operations(ctx, kind, "")
	if err != nil {
		return nil, err
	}
	var out []Record
	for _, op := range ops {
		r, err := decode(op)
		if err != nil {
			return nil, err
		}
		if r.TaskID == taskID {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out, nil
}

// LatestForTurn is the most recent attempt of a logical turn, if any.
func (s *Service) LatestForTurn(ctx context.Context, turnID string) (Record, bool, error) {
	ops, err := s.l.Operations(ctx, kind, "")
	if err != nil {
		return Record{}, false, err
	}
	var latest Record
	found := false
	for _, op := range ops {
		r, err := decode(op)
		if err != nil {
			return Record{}, false, err
		}
		if r.TurnID != turnID {
			continue
		}
		if !found || r.StartedAt.After(latest.StartedAt) {
			latest, found = r, true
		}
	}
	return latest, found, nil
}

// TakeoverAllowed says whether a previous attempt of the turn leaves work
// a new attempt may take over, and did not run in place.
func (r Record) TakeoverAllowed() bool {
	if r.Scope == ScopeUnrestricted {
		return false
	}
	switch r.State {
	case Expired, Failed, BindConflict, Superseded, Bound:
		return r.State != Superseded && r.State != Bound
	}
	return true // live but abandoned by whoever drove it
}

// History is the attempt's events.
func (s *Service) History(ctx context.Context, id string) ([]ledger.Event, error) {
	return s.l.Events(ctx, id)
}

func decode(op ledger.Operation) (Record, error) {
	var r Record
	if err := json.Unmarshal(op.Data, &r); err != nil {
		return Record{}, fmt.Errorf("attempt %s: %w", op.ID, err)
	}
	r.State = State(op.State)
	r.Revision = op.Revision
	return r, nil
}

// Describe renders a record for a card or a log line.
func Describe(r Record) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s %s", r.ID, r.Kind, r.State)
	if r.Agent != "" {
		fmt.Fprintf(&b, " by %s", r.Agent)
	}
	if r.Node != "" {
		fmt.Fprintf(&b, " on %s", r.Node)
	}
	if r.Error != "" {
		fmt.Fprintf(&b, ": %s", r.Error)
	}
	return b.String()
}

// LiveAttemptOf is the id of the task's attempt in flight, if any: what a
// side effect made on the task's behalf is claimed by.
func (s *Service) LiveAttemptOf(ctx context.Context, taskID string) (string, bool) {
	records, err := s.ForTask(ctx, taskID)
	if err != nil {
		return "", false
	}
	for i := len(records) - 1; i >= 0; i-- {
		if !records[i].State.Terminal() {
			return records[i].ID, true
		}
	}
	return "", false
}

// Reservation is capacity held ahead of an attempt: one endpoint slot,
// leased to the reservation's own id until an attempt takes it over or it
// expires.
type Reservation struct {
	ID       string       `json:"id"`
	Endpoint string       `json:"endpoint"`
	Lease    ledger.Lease `json:"lease"`
	For      string       `json:"for"` // what it was made for: plan/step
	By       string       `json:"by"`
}

const reservationKind = "reservation"

// Reserve holds one slot of (node, harness) for whoever will need it. It
// fails with NoSlot when every slot is leased.
func (s *Service) Reserve(ctx context.Context, id, node, harness string, slots int, forWhat, by string, ttl time.Duration) (Reservation, error) {
	return s.ReserveIn(ctx, "", id, node, harness, slots, forWhat, by, ttl)
}

// ReserveIn reserves in the region that issues the endpoint's leases.
func (s *Service) ReserveIn(ctx context.Context, region, id, node, harness string, slots int, forWhat, by string, ttl time.Duration) (Reservation, error) {
	if slots <= 0 {
		return Reservation{}, fmt.Errorf("attempt: %s has no slot cap; nothing to reserve", endpointKey(node, harness))
	}
	endpoint := endpointKey(node, harness)
	for i := 1; i <= slots; i++ {
		lease, err := s.l.AcquireIn(ctx, region, fmt.Sprintf("%s:slot:%d", endpoint, i), id, ttl)
		if err == nil {
			r := Reservation{ID: id, Endpoint: endpoint, Lease: lease, For: forWhat, By: by}
			return r, s.l.PutBinding(ctx, reservationKind, id, r)
		}
		if !errors.Is(err, ledger.ErrHeld) {
			return Reservation{}, err
		}
	}
	return Reservation{}, NoSlot{Endpoint: endpoint, Slots: slots}
}

// ReleaseReservation gives an unused reservation back.
func (s *Service) ReleaseReservation(ctx context.Context, id string) error {
	var r Reservation
	ok, err := s.l.GetBinding(ctx, reservationKind, id, &r)
	if err != nil || !ok {
		return err
	}
	_ = s.l.ReleaseAny(ctx, r.Lease)
	return s.l.DeleteBinding(ctx, reservationKind, id)
}

// Reservation reads one.
func (s *Service) Reservation(ctx context.Context, id string) (Reservation, bool, error) {
	var r Reservation
	ok, err := s.l.GetBinding(ctx, reservationKind, id, &r)
	return r, ok, err
}

func (s *Service) takeReservation(ctx context.Context, spec Spec, held []ledger.Lease) ([]ledger.Lease, error) {
	r, ok, err := s.Reservation(ctx, spec.Reservation)
	if err != nil {
		return held, err
	}
	if !ok {
		return held, fmt.Errorf("attempt: reservation %s does not exist", spec.Reservation)
	}
	if r.Endpoint != endpointKey(spec.Node, spec.Harness) {
		return held, fmt.Errorf("attempt: reservation %s is for %s, not %s", r.ID, r.Endpoint, endpointKey(spec.Node, spec.Harness))
	}
	if r.Lease.Region != "" && r.Lease.Region != s.l.Region() {
		return held, fmt.Errorf("attempt: reservation %s was issued by region %s; taking it over needs a transfer there", r.ID, r.Lease.Region)
	}
	lease, err := s.l.Transfer(ctx, r.Lease, spec.ID, s.TTL)
	if err != nil {
		return held, fmt.Errorf("attempt: take reservation %s: %w", r.ID, err)
	}
	lease.Region = s.l.Region()
	_ = s.l.DeleteBinding(ctx, reservationKind, r.ID)
	return append(held, lease), nil
}

const reservationForKind = "reservation-for"

// ReservationFor is the reservation made for a key such as "plan/step".
func (s *Service) ReservationFor(ctx context.Context, key string) (Reservation, bool, error) {
	var id string
	ok, err := s.l.GetBinding(ctx, reservationForKind, key, &id)
	if err != nil || !ok {
		return Reservation{}, false, err
	}
	return s.Reservation(ctx, id)
}

// ReserveFor reserves a slot and remembers which key it is for.
func (s *Service) ReserveFor(ctx context.Context, key, node, harness string, slots int, by string, ttl time.Duration) (Reservation, error) {
	return s.ReserveForIn(ctx, "", key, node, harness, slots, by, ttl)
}

// ReserveForIn is ReserveFor in a region.
func (s *Service) ReserveForIn(ctx context.Context, region, key, node, harness string, slots int, by string, ttl time.Duration) (Reservation, error) {
	id := "resv-" + strings.NewReplacer("/", "-", ":", "-").Replace(key)
	r, err := s.ReserveIn(ctx, region, id, node, harness, slots, key, by, ttl)
	if err != nil {
		return Reservation{}, err
	}
	return r, s.l.PutBinding(ctx, reservationForKind, key, id)
}

// ReleaseReservationFor releases a key's reservation if it is still held.
func (s *Service) ReleaseReservationFor(ctx context.Context, key string) {
	var id string
	if ok, err := s.l.GetBinding(ctx, reservationForKind, key, &id); err == nil && ok {
		_ = s.ReleaseReservation(ctx, id)
		_ = s.l.DeleteBinding(ctx, reservationForKind, key)
	}
}

// LeaseOf exposes a resource's lease for diagnostics.
func (s *Service) LeaseOf(ctx context.Context, key string) (ledger.Lease, bool, error) {
	return s.l.LeaseOf(ctx, key)
}

// Reservations lists every capacity reservation still held.
func (s *Service) Reservations(ctx context.Context) ([]Reservation, error) {
	raw, err := s.l.Bindings(ctx, reservationKind)
	if err != nil {
		return nil, err
	}
	out := make([]Reservation, 0, len(raw))
	for _, data := range raw {
		var r Reservation
		if err := json.Unmarshal(data, &r); err == nil {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
