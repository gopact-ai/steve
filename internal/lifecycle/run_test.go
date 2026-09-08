package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/ability"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/roster"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/view"
)

// fakeAttempts is the ledger's shape without the ledger: one record, and
// the order everything happened to it.
type fakeAttempts struct {
	mu      sync.Mutex
	record  attempt.Record
	events  []string
	lost    chan struct{}
	openErr error
	// openErrs are answered one per open, before openErr.
	openErrs     []error
	supersedeErr error
	failAt       attempt.State
	// unsettledErr fails the quarantine marker.
	unsettledErr error
	// ttl, when set, makes the lease real: Heartbeat renews it every
	// ttl/3, and a transition on an expired lease is lost.
	ttl      time.Duration
	expires  time.Time
	renewals int
	// beat is the heartbeat's context, so a transition can tell whether
	// the lease was still being renewed when it ran; terminal transitions
	// that found it so are kept in beating.
	beat    context.Context
	beating []attempt.State
}

func (f *fakeAttempts) log(event string) {
	f.mu.Lock()
	f.events = append(f.events, event)
	f.mu.Unlock()
}
func (f *fakeAttempts) history() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.events, " ")
}
func (f *fakeAttempts) Heartbeat(ctx context.Context, _ string) <-chan struct{} {
	f.mu.Lock()
	f.beat = ctx
	ttl := f.ttl
	f.mu.Unlock()
	if ttl > 0 {
		go func() {
			ticker := time.NewTicker(ttl / 3)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					f.mu.Lock()
					f.expires = time.Now().Add(ttl)
					f.renewals++
					f.mu.Unlock()
				}
			}
		}()
	}
	return f.lost
}

// renewing says the heartbeat is still running.
func (f *fakeAttempts) renewing() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.beat != nil && f.beat.Err() == nil
}

// held says the lease has not expired.
func (f *fakeAttempts) held() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ttl > 0 && time.Now().Before(f.expires)
}
func (f *fakeAttempts) renewed() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.renewals
}
func (f *fakeAttempts) Open(_ context.Context, spec attempt.Spec) (attempt.Record, error) {
	f.log("open")
	if len(f.openErrs) > 0 {
		err := f.openErrs[0]
		f.openErrs = f.openErrs[1:]
		if err != nil {
			return attempt.Record{}, err
		}
	} else if f.openErr != nil {
		return attempt.Record{}, f.openErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record = attempt.Record{Spec: spec, State: attempt.Leased}
	if f.record.ID == "" {
		f.record.ID = "a1"
	}
	if f.ttl > 0 {
		f.expires = time.Now().Add(f.ttl)
	}
	return f.record, nil
}
func (f *fakeAttempts) Get(context.Context, string) (attempt.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.record, nil
}
func (f *fakeAttempts) Advance(_ context.Context, _ string, to attempt.State, actor string, mutate func(*attempt.Record)) (attempt.Record, error) {
	f.log(string(to) + "/" + actor)
	if to == f.failAt {
		return attempt.Record{}, errors.New("ledger refused " + string(to))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ttl > 0 && time.Now().After(f.expires) {
		return attempt.Record{}, fmt.Errorf("%w: lease expired before %s", attempt.ErrLost, to)
	}
	if to.Terminal() && f.beat != nil && f.beat.Err() == nil {
		f.beating = append(f.beating, to)
	}
	next := f.record
	next.State = to
	if mutate != nil {
		mutate(&next)
	}
	f.record = next
	return next, nil
}
func (f *fakeAttempts) ArmSession(_ context.Context, _, actor string) error {
	f.log("arm/" + actor)
	return nil
}
func (f *fakeAttempts) MarkSessionSettled(_ context.Context, _, actor string) error {
	f.log("settled/" + actor)
	return nil
}
func (f *fakeAttempts) MarkUnsettled(_ context.Context, _, actor string, cause error, usage *attempt.Usage) error {
	f.log("unsettled/" + actor)
	if f.unsettledErr != nil {
		return f.unsettledErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record.Unsettled = true
	f.record.Error = cause.Error()
	f.record.Usage = usage
	return nil
}
func (f *fakeAttempts) FinishCompletion(ctx context.Context, id, actor string, completion attempt.Completion) (attempt.Record, error) {
	f.log("finish")
	// As the ledger's: a record already bind-ready commits the result it
	// carries, not the one it was handed.
	f.mu.Lock()
	if f.record.State == attempt.BindReady && f.record.Result != nil {
		completion.Result = *f.record.Result
	}
	f.mu.Unlock()
	return f.bind(ctx, id, actor, completion)
}
func (f *fakeAttempts) Complete(ctx context.Context, id, actor string, completion attempt.Completion) (attempt.Record, error) {
	f.log("complete")
	return f.bind(ctx, id, actor, completion)
}
func (f *fakeAttempts) bind(ctx context.Context, id, actor string, completion attempt.Completion) (attempt.Record, error) {
	return f.Advance(ctx, id, attempt.Bound, actor, func(r *attempt.Record) { r.Result = &completion.Result; r.Usage = completion.Usage })
}
func (f *fakeAttempts) Supersede(_ context.Context, oldID string, spec attempt.Spec, _ string) (attempt.Record, error) {
	f.log("supersede/" + oldID)
	if f.supersedeErr != nil {
		return attempt.Record{}, f.supersedeErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record = attempt.Record{Spec: spec, State: attempt.Leased}
	if f.record.ID == "" {
		f.record.ID = "a1"
	}
	return f.record, nil
}
func (f *fakeAttempts) RejectCompletion(ctx context.Context, id, actor string, completion attempt.Completion, cause error) error {
	f.log("reject")
	_, err := f.Advance(ctx, id, attempt.Failed, actor, func(r *attempt.Record) { r.Error = cause.Error(); r.Result = &completion.Result })
	return errors.Join(cause, err)
}
func (f *fakeAttempts) FailWith(ctx context.Context, id, actor, cause string, usage *attempt.Usage) (attempt.Record, error) {
	return f.Advance(ctx, id, attempt.Failed, actor, func(r *attempt.Record) { r.Error = cause; r.Usage = usage })
}

type fakeRoster struct {
	verdict  ability.Verdict
	bindings []ability.Binding
	admits   int
	released []string
	log      func(string)
}

func (f *fakeRoster) Admit(context.Context, roster.Candidate, []string, []string, string) (ability.Admission, []ability.Binding, error) {
	f.admits++
	f.log("admit")
	return ability.Admission{Verdict: f.verdict}, f.bindings, nil
}
func (f *fakeRoster) Release(_ context.Context, node, id string) {
	f.log("release")
	f.released = append(f.released, node+"/"+id)
}

type fakeSessions struct {
	runner   *runner
	openErr  error
	closeErr error
	opened   int
	closed   []string
	log      func(string)
}

func (f *fakeSessions) OpenSession(context.Context, harness.Placement, string, string, []acp.MCPServer) (harness.Runner, error) {
	f.log("session")
	if f.openErr != nil {
		return nil, f.openErr
	}
	f.opened++
	return f.runner, nil
}
func (f *fakeSessions) CloseSession(_ context.Context, _ harness.Placement, id string) error {
	f.log("close")
	f.closed = append(f.closed, id)
	return f.closeErr
}

type fakeWorkspaces struct {
	discarded int
	// live says the last discard's context was not cancelled; remaining
	// is how much of its deadline it had.
	live      bool
	remaining time.Duration
	log       func(string)
}

func (f *fakeWorkspaces) Discard(ctx context.Context, _ project.Workspace) error {
	f.log("discard")
	f.discarded++
	f.live = ctx.Err() == nil
	if deadline, ok := ctx.Deadline(); ok {
		f.remaining = time.Until(deadline)
	}
	return nil
}

type world struct {
	attempts   *fakeAttempts
	roster     *fakeRoster
	sessions   *fakeSessions
	workspaces *fakeWorkspaces
	runner     *runner
}

func newWorld(sessionID string) *world {
	att := &fakeAttempts{lost: make(chan struct{})}
	r := &runner{id: sessionID, progress: []view.Progress{{Settings: view.Settings{Model: "m"}, Usage: view.Usage{InputTokens: 5, OutputTokens: 7}}}}
	return &world{attempts: att, roster: &fakeRoster{verdict: ability.True, log: att.log}, sessions: &fakeSessions{runner: r, log: att.log}, workspaces: &fakeWorkspaces{log: att.log}, runner: r}
}

// withWorld is o against another world's fakes.
func (o Options) withWorld(w *world) Options {
	o.Attempts, o.Roster, o.Sessions, o.Workspaces = w.attempts, w.roster, w.sessions, w.workspaces
	return o
}

func (w *world) options() Options {
	return Options{
		Attempts: w.attempts, Roster: w.roster, Sessions: w.sessions, Workspaces: w.workspaces, Actor: "test",
		Spec:     attempt.Spec{ID: "a1", TaskID: "t", Node: "n1", Agent: "agent", Workspace: project.Workspace{ID: "ws"}},
		ArmActor: "test-open", Prompt: "hi",
		Finish: func(_ context.Context, e *Execution) (attempt.Completion, error) {
			return attempt.Completion{Result: attempt.Result{Summary: e.Outcome.Answer}}, nil
		},
	}
}

func TestRunTakesAnAttemptFromOpenToClosedInOrder(t *testing.T) {
	w := newWorld("s1")
	w.roster.bindings = []ability.Binding{{Name: "tool"}}
	var prepared, started bool
	o := w.options()
	o.Prepare = func(_ context.Context, e *Execution) (func(*attempt.Record), error) {
		prepared = e.Record.State == attempt.Leased && len(e.Bindings) == 1
		e.Prompt = "sys\n\n" + e.Prompt
		return func(r *attempt.Record) { r.Base = "b0" }, nil
	}
	o.Started = func(_ context.Context, e *Execution) error {
		started = e.Record.State == attempt.Running && e.Record.Session == "s1"
		return nil
	}
	res, err := Run(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	want := "open admit prepared/test arm/test-open session running/test close settled/test release finish bound/test discard"
	if got := w.attempts.history(); got != want {
		t.Fatalf("order:\n got %s\nwant %s", got, want)
	}
	if !prepared || !started || !res.Durable || res.Unsettled || !res.Settled || !res.Driven || res.Managed {
		t.Fatalf("result = %+v prepared=%v started=%v", res, prepared, started)
	}
	if res.Record.State != attempt.Bound || res.Record.Base != "b0" || res.Record.Admission == nil || res.Record.Result.Summary != "plain" || res.Usage == nil || res.Usage.Input != 5 || res.Record.Usage != res.Usage {
		t.Fatalf("record = %+v usage=%+v", res.Record, res.Usage)
	}
	if len(w.roster.released) != 1 || w.roster.released[0] != "n1/a1" || len(w.sessions.closed) != 1 || w.workspaces.discarded != 1 {
		t.Fatalf("cleanup: released=%v closed=%v discarded=%d", w.roster.released, w.sessions.closed, w.workspaces.discarded)
	}
}

func TestRunFailsARefusedAdmissionBeforeAnySession(t *testing.T) {
	w := newWorld("s1")
	w.roster.verdict = ability.False
	res, err := Run(t.Context(), w.options())
	var step *StepError
	var refused *Refused
	if !errors.As(err, &step) || step.Step != StepAdmit || !errors.As(err, &refused) {
		t.Fatalf("refusal = %v", err)
	}
	if res.Record.State != attempt.Failed || !res.Durable || w.sessions.opened != 0 || w.workspaces.discarded != 1 || res.Record.Error != refused.Error() {
		t.Fatalf("result = %+v opened=%d discarded=%d", res, w.sessions.opened, w.workspaces.discarded)
	}
	unsure := newWorld("s1")
	unsure.roster.verdict = ability.Unsure
	if _, err := Run(t.Context(), unsure.options()); !errors.As(err, &refused) {
		t.Fatalf("unsure ran without leave: %v", err)
	}
	o := unsure.options()
	o.AdmitUnsure = true
	if _, err := Run(t.Context(), o); err != nil {
		t.Fatalf("unsure with leave = %v", err)
	}
}

func TestRunFailsWhenTheSessionDoesNotOpen(t *testing.T) {
	w := newWorld("s1")
	w.sessions.openErr = errors.New("no agent")
	res, err := Run(t.Context(), w.options())
	var step *StepError
	if !errors.As(err, &step) || step.Step != StepSession || !errors.Is(err, w.sessions.openErr) {
		t.Fatalf("open failure = %v", err)
	}
	if res.Record.State != attempt.Failed || res.Driven || !strings.Contains(w.attempts.history(), "settled/test-open-rejected") || w.workspaces.discarded != 1 {
		t.Fatalf("rejected open: %+v history=%s", res, w.attempts.history())
	}
	uncertain := newWorld("s1")
	uncertain.sessions.openErr = errors.Join(harness.ErrStopUnconfirmed, errors.New("reply lost"))
	res, err = Run(t.Context(), uncertain.options())
	if !errors.Is(err, harness.ErrStopUnconfirmed) || !res.Unsettled || res.Record.State != attempt.Prepared || !res.Record.Unsettled || uncertain.workspaces.discarded != 0 {
		t.Fatalf("uncertain open was not quarantined: %+v err=%v", res, err)
	}
	if h := uncertain.attempts.history(); strings.Contains(h, "rejected") || !strings.HasSuffix(h, "unsettled/test") {
		t.Fatalf("uncertain open history = %s", h)
	}
}

func TestRunReadsAnUnsettledPromptByTheCallersRule(t *testing.T) {
	w := newWorld("s1")
	w.runner.err = errors.New("connection lost")
	o := w.options()
	o.Settlement.Quarantine = QuarantineAlways
	res, err := Run(t.Context(), o)
	if !errors.Is(err, harness.ErrStopUnconfirmed) || !errors.Is(err, w.runner.err) || !res.Unsettled || res.Record.State != attempt.Running || res.Settled {
		t.Fatalf("unsettled prompt was not quarantined: %+v err=%v", res, err)
	}
	if len(w.sessions.closed) != 0 || w.workspaces.discarded != 0 || len(w.roster.released) != 0 || res.Record.Usage == nil {
		t.Fatalf("quarantine gave something back: closed=%v discarded=%d released=%v", w.sessions.closed, w.workspaces.discarded, w.roster.released)
	}
	plain := newWorld("s1")
	plain.runner.err = errors.New("connection lost")
	res, err = Run(t.Context(), plain.options())
	if errors.Is(err, harness.ErrStopUnconfirmed) || res.Unsettled || res.Record.State != attempt.Failed || len(plain.sessions.closed) != 1 || plain.workspaces.discarded != 1 {
		t.Fatalf("a closed session's failure was quarantined: %+v err=%v", res, err)
	}
	if strings.Contains(plain.attempts.history(), "settled/test") {
		t.Fatal("an unsettled prompt was recorded settled")
	}
	stopped := newWorld("s1")
	stopped.runner.err = errors.New("connection lost")
	stopped.runner.stopped = true
	o = stopped.options()
	o.Settlement = Settlement{StoppedSettles: true, Quarantine: QuarantineAlways}
	res, err = Run(t.Context(), o)
	if errors.Is(err, harness.ErrStopUnconfirmed) || !res.Settled || res.Record.State != attempt.Failed || !strings.Contains(stopped.attempts.history(), "settled/test") {
		t.Fatalf("a stopped process did not settle: %+v err=%v", res, err)
	}
}

func TestRunCancelsTheWorkWhenTheLeaseIsLost(t *testing.T) {
	w := newWorld("s1")
	w.runner.block = true
	o := w.options()
	var lost atomic.Bool
	o.Lost = func() { lost.Store(true) }
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(w.attempts.lost)
	}()
	res, err := Run(t.Context(), o)
	if !errors.Is(err, context.Canceled) || !lost.Load() || res.Record.State != attempt.Failed || !res.Durable {
		t.Fatalf("lost lease: %+v err=%v lost=%v", res, err, lost.Load())
	}
	if !strings.Contains(res.Record.Error, context.Canceled.Error()) {
		t.Fatalf("recorded cause = %q", res.Record.Error)
	}
}

func TestRunRenewsTheLeaseUntilTheTerminalTransition(t *testing.T) {
	// The caller's completion may take longer than the lease has left;
	// the lease is renewed through it and stops only for the transition
	// it fences.
	const ttl = 150 * time.Millisecond
	w := newWorld("s1")
	w.attempts.ttl = ttl
	o := w.options()
	var renewing, held bool
	o.Finish = func(_ context.Context, e *Execution) (attempt.Completion, error) {
		renewing = w.attempts.renewing()
		time.Sleep(4 * ttl)
		held = w.attempts.held()
		return attempt.Completion{Result: attempt.Result{Summary: e.Outcome.Answer}}, nil
	}
	res, err := Run(t.Context(), o)
	if err != nil || res.Record.State != attempt.Bound || !res.Durable {
		t.Fatalf("a slow completion lost its lease: %+v err=%v", res, err)
	}
	if !renewing || !held || w.attempts.renewed() < 2 {
		t.Fatalf("the lease was not renewed through the completion: renewing=%v held=%v renewals=%d", renewing, held, w.attempts.renewed())
	}
	if len(w.attempts.beating) != 0 || w.attempts.renewing() {
		t.Fatalf("the heartbeat outlived the terminal transition: %v", w.attempts.beating)
	}
	failed := newWorld("s1")
	failed.attempts.ttl = ttl
	failed.runner.err = errors.New("boom")
	o = failed.options()
	o.Failed = func(*Execution, error) (*attempt.Result, error) {
		time.Sleep(4 * ttl)
		return &attempt.Result{Summary: "partial"}, nil
	}
	res, err = Run(t.Context(), o)
	if !errors.Is(err, failed.runner.err) || res.Record.State != attempt.Failed || res.Record.Result == nil || res.Record.Result.Summary != "partial" {
		t.Fatalf("a slow failure hook lost its lease: %+v err=%v", res, err)
	}
	if len(failed.attempts.beating) != 0 || failed.attempts.renewed() < 2 {
		t.Fatalf("the heartbeat outlived the failure: beating=%v renewals=%d", failed.attempts.beating, failed.attempts.renewed())
	}
	managed := newWorld("ns_1")
	managed.attempts.ttl = ttl
	o = managed.options()
	o.Settlement = Settlement{Quarantine: QuarantineManaged, DetachManaged: true}
	o.Finish = func(_ context.Context, e *Execution) (attempt.Completion, error) {
		time.Sleep(4 * ttl)
		return attempt.Completion{Result: attempt.Result{Summary: e.Outcome.Answer}}, nil
	}
	res, err = Run(t.Context(), o)
	if err != nil || res.Record.State != attempt.Bound || len(managed.attempts.beating) != 0 {
		t.Fatalf("a node-owned session's slow completion: %+v err=%v beating=%v", res, err, managed.attempts.beating)
	}
}

func TestRunCleansUpOnAWindowMintedAfterTheCallersHooks(t *testing.T) {
	// The completion may take most of what a cleanup is allowed; what
	// follows it — the transition, the discard — gets a window of its own.
	const took = 200 * time.Millisecond
	w := newWorld("s1")
	o := w.options()
	o.Finish = func(_ context.Context, e *Execution) (attempt.Completion, error) {
		time.Sleep(took)
		return attempt.Completion{Result: attempt.Result{Summary: e.Outcome.Answer}}, nil
	}
	if _, err := Run(t.Context(), o); err != nil {
		t.Fatal(err)
	}
	if w.workspaces.remaining < CleanupTimeout-took/2 {
		t.Fatalf("the discard ran on the completion's leftovers: %v of %v left", w.workspaces.remaining, CleanupTimeout)
	}
	failed := newWorld("s1")
	o = failed.options()
	o.Finish = func(context.Context, *Execution) (attempt.Completion, error) {
		time.Sleep(took)
		return attempt.Completion{}, errors.New("no completion")
	}
	if _, err := Run(t.Context(), o); err == nil || failed.attempts.record.State != attempt.Failed {
		t.Fatalf("failed completion = %v state=%s", err, failed.attempts.record.State)
	}
	if failed.workspaces.remaining < CleanupTimeout-took/2 {
		t.Fatalf("the failure's discard ran on the completion's leftovers: %v of %v left", failed.workspaces.remaining, CleanupTimeout)
	}
	// A workspace whose attempt never opened is given back even when the
	// run was cancelled waiting for it.
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	unopened := newWorld("s1")
	unopened.attempts.openErr = errors.New("no lease")
	if _, err := Run(cancelled, unopened.options()); !errors.Is(err, unopened.attempts.openErr) {
		t.Fatalf("open failure = %v", err)
	}
	if unopened.workspaces.discarded != 1 || !unopened.workspaces.live {
		t.Fatalf("an unopened attempt's workspace was discarded on the cancelled run: discarded=%d live=%v", unopened.workspaces.discarded, unopened.workspaces.live)
	}
}

func TestRunReportsAQuarantineItCouldNotWrite(t *testing.T) {
	w := newWorld("ns_1")
	w.runner.err = errors.New("observer lost")
	w.attempts.unsettledErr = errors.New("ledger away")
	o := w.options()
	o.Settlement = Settlement{Quarantine: QuarantineManaged, DetachManaged: true, Detachment: DetachQuarantines}
	res, err := Run(t.Context(), o)
	var detached *execution.RetainedObserverDetached
	if !errors.As(err, &detached) || !errors.Is(err, w.attempts.unsettledErr) || !errors.Is(err, w.runner.err) || !res.Unsettled || res.Record.Unsettled {
		t.Fatalf("a quarantine that failed was not reported: %+v err=%v", res, err)
	}
}

func TestRunDetachesFromAManagedSessionItCannotVouchFor(t *testing.T) {
	w := newWorld("ns_1")
	w.runner.err = errors.New("observer lost")
	o := w.options()
	o.Settlement = Settlement{Quarantine: QuarantineManaged, DetachManaged: true}
	res, err := Run(t.Context(), o)
	var detached *execution.RetainedObserverDetached
	if !errors.As(err, &detached) || detached.AttemptID != "a1" || detached.SessionID != "ns_1" || !errors.Is(err, harness.ErrStopUnconfirmed) {
		t.Fatalf("detachment = %v", err)
	}
	if !res.Managed || res.Unsettled || res.Record.State != attempt.Running || len(w.sessions.closed) != 0 || w.workspaces.discarded != 0 || strings.Contains(w.attempts.history(), "unsettled") {
		t.Fatalf("silent detachment touched the record: %+v history=%s", res, w.attempts.history())
	}
	loud := newWorld("ns_1")
	loud.runner.err = errors.New("observer lost")
	o = loud.options()
	o.Settlement = Settlement{Quarantine: QuarantineManaged, DetachManaged: true, Detachment: DetachQuarantines}
	res, err = Run(t.Context(), o)
	if !errors.As(err, &detached) || !res.Unsettled || !res.Record.Unsettled {
		t.Fatalf("detachment did not quarantine: %+v err=%v", res, err)
	}
	done := newWorld("ns_1")
	o = done.options()
	o.Settlement = Settlement{Quarantine: QuarantineManaged, DetachManaged: true}
	res, err = Run(t.Context(), o)
	if err != nil || res.Record.State != attempt.Bound || !res.Durable || len(done.sessions.closed) != 1 || done.workspaces.discarded != 1 {
		t.Fatalf("a settled managed prompt did not complete and clean up: %+v err=%v", res, err)
	}
	if want := "open admit prepared/test arm/test-open session running/test settled/test finish bound/test close discard"; done.attempts.history() != want {
		t.Fatalf("managed order = %s", done.attempts.history())
	}
	unpublished := newWorld("ns_1")
	unpublished.attempts.failAt = attempt.Running
	o = unpublished.options()
	o.Settlement = Settlement{DetachManaged: true, QuarantineUnpublished: true}
	res, err = Run(t.Context(), o)
	if !errors.Is(err, harness.ErrStopUnconfirmed) || !res.Unsettled || res.Driven || res.Record.Session != "" || len(unpublished.sessions.closed) != 0 {
		t.Fatalf("unpublished identity was not quarantined: %+v err=%v", res, err)
	}
}

func TestRunQuarantinesAManagedSessionWhoseCloseIsUnconfirmed(t *testing.T) {
	// The completion is on the record, but the close did not confirm the
	// process exited: the workspace, the slot and the bindings stay, and
	// the record says the writer is unconfirmed.
	w := newWorld("ns_1")
	w.roster.bindings = []ability.Binding{{Name: "tool"}}
	w.sessions.closeErr = errors.New("node away")
	o := w.options()
	o.Settlement = Settlement{Quarantine: QuarantineManaged, DetachManaged: true}
	res, err := Run(t.Context(), o)
	if err != nil || res.Record.State != attempt.Bound || res.Durable || !res.Unsettled || !res.Record.Unsettled || !errors.Is(res.CleanupErr, harness.ErrStopUnconfirmed) {
		t.Fatalf("unconfirmed close: %+v err=%v", res, err)
	}
	if w.workspaces.discarded != 0 || len(w.roster.released) != 0 || len(w.sessions.closed) != 1 {
		t.Fatalf("an unconfirmed writer's things were given back: discarded=%d released=%v closed=%v", w.workspaces.discarded, w.roster.released, w.sessions.closed)
	}
	if want := "open admit prepared/test arm/test-open session running/test settled/test finish bound/test close unsettled/test"; w.attempts.history() != want {
		t.Fatalf("order = %s", w.attempts.history())
	}
	stopped := newWorld("ns_1")
	stopped.roster.bindings = []ability.Binding{{Name: "tool"}}
	stopped.sessions.closeErr = errors.New("node away")
	stopped.runner.stopped = true
	res, err = Run(t.Context(), o.withWorld(stopped))
	if err != nil || !res.Durable || res.Unsettled || stopped.workspaces.discarded != 1 || len(stopped.roster.released) != 1 {
		t.Fatalf("a stopped process's failed close was quarantined: %+v err=%v discarded=%d released=%v", res, err, stopped.workspaces.discarded, stopped.roster.released)
	}
}

func TestRunKeepsAConversationsSessionAndRejectsAnUnfinishedCompletion(t *testing.T) {
	w := newWorld("s1")
	o := w.options()
	o.Settlement.KeepSession = true
	o.Arm = func(context.Context, *Execution) (func(*attempt.Record), error) {
		return func(r *attempt.Record) { r.Preferences = &attempt.SessionPreferences{Model: "m"} }, nil
	}
	res, err := Run(t.Context(), o)
	if err != nil || len(w.sessions.closed) != 0 || res.Record.Preferences == nil {
		t.Fatalf("kept session: %+v closed=%v err=%v", res, w.sessions.closed, err)
	}
	early := newWorld("s1")
	early.attempts.failAt = attempt.Running
	o = early.options()
	o.Settlement.KeepSession = true
	o.Arm = func(context.Context, *Execution) (func(*attempt.Record), error) {
		return nil, errors.New("state unavailable")
	}
	if _, err := Run(t.Context(), o); err == nil || len(early.sessions.closed) != 1 {
		t.Fatalf("a session the conversation never took was kept: closed=%v err=%v", early.sessions.closed, err)
	}
	partial := newWorld("s1")
	o = partial.options()
	o.Finish = func(context.Context, *Execution) (attempt.Completion, error) {
		return attempt.Completion{}, &Rejected{Completion: attempt.Completion{Result: attempt.Result{Summary: "half"}}, Cause: errors.New("snapshot unavailable")}
	}
	res, err = Run(t.Context(), o)
	var step *StepError
	if !errors.As(err, &step) || step.Step != StepFinish || res.Record.State != attempt.Failed || res.Record.Result.Summary != "half" || partial.workspaces.discarded != 0 || res.Durable {
		t.Fatalf("rejected completion: %+v err=%v", res, err)
	}
}

func TestReattachCommitsWhatTheNodeFinishedUnderACancelledObserver(t *testing.T) {
	// The node finished the prompt before the observer was cancelled: a
	// retained chat turn commits the answer; a caller that detaches on
	// cancellation leaves the record for the observer that comes back.
	running := attempt.Record{Spec: attempt.Spec{ID: "a1", TaskID: "t", Node: "n1", Workspace: project.Workspace{ID: "ws"}}, State: attempt.Running, Session: "ns_1"}
	w := newWorld("ns_1")
	w.attempts.record = running
	ctx, cancel := context.WithCancel(t.Context())
	w.runner.during = cancel
	o := w.options()
	o.Resume = true
	o.Settlement = Settlement{DetachManaged: true, Detachment: DetachQuarantinesUnlessCancelled, RejectManaged: true, KeepSession: true}
	res, err := Reattach(ctx, o, running, w.runner)
	if err != nil || res.Record.State != attempt.Bound || res.Record.Result == nil || res.Record.Result.Summary != "resumed" {
		t.Fatalf("a finished prompt was not committed under a cancelled observer: %+v err=%v", res, err)
	}
	if h := w.attempts.history(); !strings.Contains(h, "finish bound/test") || strings.Contains(h, "failed") {
		t.Fatalf("history = %s", h)
	}
	leaves := newWorld("ns_1")
	leaves.attempts.record = running
	ctx, cancel = context.WithCancel(t.Context())
	leaves.runner.during = cancel
	o = leaves.options()
	o.Resume = true
	o.Settlement = Settlement{Quarantine: QuarantineAlways, DetachManaged: true, CancelDetaches: true}
	res, err = Reattach(ctx, o, running, leaves.runner)
	var detached *execution.RetainedObserverDetached
	if !errors.As(err, &detached) || !errors.Is(err, context.Canceled) || res.Record.State != attempt.Running || strings.Contains(leaves.attempts.history(), "failed") {
		t.Fatalf("a cancelled run that detaches was recorded: %+v err=%v history=%s", res, err, leaves.attempts.history())
	}
}

func TestReattachRecordsAnExplicitStopOfASettledPromptAsCancelled(t *testing.T) {
	// The task was stopped while the node finished the prompt: the
	// attempt is failed as a cancelled turn, on a context the stop did
	// not cancel, with what the caller keeps of the answer.
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	tasks, err := task.OpenLedger(book, "")
	if err != nil {
		t.Fatal(err)
	}
	registry := execution.New(t.Context(), tasks)
	for _, tc := range []struct {
		name       string
		settlement Settlement
		failed     func(*Execution, error) (*attempt.Result, error)
	}{
		{"retained chat turn", Settlement{DetachManaged: true, Detachment: DetachQuarantinesUnlessCancelled, RejectManaged: true, KeepSession: true}, nil},
		{"delegation", Settlement{Quarantine: QuarantineManaged, DetachManaged: true, Detachment: DetachQuarantinesUnlessCancelled, CancelDetaches: true}, func(e *Execution, cause error) (*attempt.Result, error) {
			return &attempt.Result{Summary: e.Outcome.Answer}, nil
		}},
	} {
		work, err := tasks.Create(task.Task{Member: "agent", ProjectID: "p"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tasks.Begin(work.ID, "agent", "n1", "ns_1"); err != nil {
			t.Fatal(err)
		}
		token, err := tasks.ExecutionToken(work.ID)
		if err != nil {
			t.Fatal(err)
		}
		running := attempt.Record{Spec: attempt.Spec{ID: "a1", TaskID: work.ID, Node: "n1", Workspace: project.Workspace{ID: "ws"}, Execution: &token}, State: attempt.Running, Session: "ns_1"}
		scope, err := registry.BeginAccepted(t.Context(), execution.Key{TaskID: work.ID, InstanceID: "turn", AttemptID: running.ID}, &token)
		if err != nil {
			t.Fatal(err)
		}
		w := newWorld("ns_1")
		w.attempts.record = running
		w.runner.during = func() {
			ids, err := tasks.SetAside(work.ID, task.StatePaused)
			if err != nil {
				t.Error(err)
			}
			registry.Stop(ids, task.ErrExecutionStopped)
		}
		o := w.options()
		o.Resume, o.Settlement, o.Failed = true, tc.settlement, tc.failed
		res, err := Reattach(scope.Context(), o, running, w.runner)
		scope.Finish(nil)
		if !errors.Is(err, harness.ErrTurnCanceled) || res.Record.State != attempt.Failed || !strings.Contains(res.Record.Error, harness.ErrTurnCanceled.Error()) || !res.Durable {
			t.Fatalf("%s: explicit stop of a settled prompt: %+v err=%v", tc.name, res, err)
		}
		if h := w.attempts.history(); !strings.Contains(h, "settled/test failed/test") {
			t.Fatalf("%s: history = %s", tc.name, h)
		}
		if tc.failed != nil && (res.Record.Result == nil || res.Record.Result.Summary != "resumed") {
			t.Fatalf("%s: the answer was dropped: %+v", tc.name, res.Record)
		}
	}
}

func TestReattachJoinsAnExecutionFromThePromptOn(t *testing.T) {
	w := newWorld("ns_1")
	running := attempt.Record{Spec: attempt.Spec{ID: "a1", TaskID: "t", Node: "n1", Workspace: project.Workspace{ID: "ws"}}, State: attempt.Running, Session: "ns_1", Admission: &ability.Admission{Bound: []string{"tool"}}}
	w.attempts.record = running
	o := w.options()
	o.Resume = true
	o.Settlement = Settlement{Quarantine: QuarantineAlways, DetachManaged: true, CancelDetaches: true}
	res, err := Reattach(t.Context(), o, running, w.runner)
	if err != nil || w.runner.resumes != 1 || w.runner.prompts != 0 || res.Record.State != attempt.Bound || res.Answer != "resumed" || !res.Durable {
		t.Fatalf("reattach = %+v err=%v resumes=%d prompts=%d", res, err, w.runner.resumes, w.runner.prompts)
	}
	if want := "settled/test finish bound/test close release discard"; w.attempts.history() != want {
		t.Fatalf("reattach order = %s", w.attempts.history())
	}
	replay := newWorld("ns_1")
	ended := running
	ended.State, ended.Usage = attempt.BindReady, &attempt.Usage{Input: 9}
	replay.attempts.record = ended
	o = replay.options()
	o.Replay = &Outcome{Answer: "from the record", PromptSettled: true}
	o.Settlement = Settlement{DetachManaged: true}
	res, err = Reattach(t.Context(), o, ended, replay.runner)
	if err != nil || replay.runner.resumes != 0 || replay.runner.prompts != 0 || res.Record.State != attempt.Bound || res.Record.Result.Summary != "from the record" || res.Usage.Input != 9 {
		t.Fatalf("replay = %+v err=%v", res, err)
	}
	lost := newWorld("ns_1")
	lost.attempts.record = running
	lost.runner.err = errors.New("observer lost")
	o = lost.options()
	o.Resume = true
	o.Settlement = Settlement{Quarantine: QuarantineAlways, DetachManaged: true, Detachment: DetachQuarantines}
	res, err = Reattach(t.Context(), o, running, lost.runner)
	var detached *execution.RetainedObserverDetached
	if !errors.As(err, &detached) || detached.SessionID != "ns_1" || !res.Unsettled || res.Record.State != attempt.Running {
		t.Fatalf("lost observer = %+v err=%v", res, err)
	}
	kept := newWorld("ns_1")
	kept.attempts.record = running
	kept.attempts.failAt = attempt.Bound
	o = kept.options()
	o.Resume = true
	o.Settlement = Settlement{DetachManaged: true, RejectManaged: true, KeepSession: true}
	res, err = Reattach(t.Context(), o, running, kept.runner)
	var step *StepError
	if !errors.As(err, &step) || step.Step != StepFinish || errors.As(err, &detached) || res.Record.State != attempt.Failed || len(kept.sessions.closed) != 0 {
		t.Fatalf("a chat turn's uncommittable completion was not rejected: %+v err=%v", res, err)
	}
}

func TestReattachReleasesARetainedSessionsBindingsOnEveryTerminalTransition(t *testing.T) {
	// A retained chat turn closes its own attempt: whether the completion
	// is committed, rejected or failed, the record is terminal and no
	// observer comes back for the bindings the node bound for it. Only a
	// detachment — and the quarantine of a stop nobody confirmed — leaves
	// them for the observer that does.
	running := attempt.Record{Spec: attempt.Spec{ID: "a1", TaskID: "t", Node: "n1", Workspace: project.Workspace{ID: "ws"}}, State: attempt.Running, Session: "ns_1", Admission: &ability.Admission{Bound: []string{"tool"}}}
	retained := Settlement{DetachManaged: true, Detachment: DetachQuarantinesUnlessCancelled, RejectManaged: true, KeepSession: true}
	for _, tc := range []struct {
		name     string
		arrange  func(w *world, o *Options)
		history  string
		released bool
	}{
		{"committed", func(*world, *Options) {}, "settled/test finish bound/test release discard", true},
		{"Finish rejects it", func(_ *world, o *Options) {
			o.Finish = func(context.Context, *Execution) (attempt.Completion, error) {
				return attempt.Completion{}, &Rejected{Completion: attempt.Completion{Result: attempt.Result{Summary: "half"}}, Cause: errors.New("snapshot unavailable")}
			}
		}, "settled/test reject failed/test release", true},
		{"the ledger rejects the completion", func(w *world, _ *Options) { w.attempts.failAt = attempt.Bound }, "settled/test finish bound/test reject failed/test release", true},
		{"Finish fails", func(_ *world, o *Options) {
			o.Finish = func(context.Context, *Execution) (attempt.Completion, error) {
				return attempt.Completion{}, errors.New("no result")
			}
		}, "settled/test failed/test discard release", true},
		{"the observer detaches", func(w *world, o *Options) {
			w.runner.err = harness.ErrStopUnconfirmed
			o.Settlement.Detachment = DetachSilently
		}, "", false},
		{"the detachment quarantines", func(w *world, _ *Options) { w.runner.err = harness.ErrStopUnconfirmed }, "unsettled/test", false},
	} {
		w := newWorld("ns_1")
		w.attempts.record = running
		o := w.options()
		o.Resume = true
		o.Settlement = retained
		tc.arrange(w, &o)
		res, err := Reattach(t.Context(), o, running, w.runner)
		if h := w.attempts.history(); h != tc.history {
			t.Fatalf("%s: history = %q, want %q (err=%v)", tc.name, h, tc.history, err)
		}
		var detached *execution.RetainedObserverDetached
		if errors.As(err, &detached) == tc.released || res.Record.State.Terminal() != tc.released {
			t.Fatalf("%s: %+v err=%v", tc.name, res.Record, err)
		}
		if released := len(w.roster.released) == 1 && w.roster.released[0] == "n1/a1"; released != tc.released || len(w.roster.released) > 1 {
			t.Fatalf("%s: released = %v, want released=%v", tc.name, w.roster.released, tc.released)
		}
		if len(w.sessions.closed) != 0 {
			t.Fatalf("%s: a kept session was closed: %v", tc.name, w.sessions.closed)
		}
	}
}

func TestRunTakesOverThePreviousAttemptAndOpensAfterAFullSlot(t *testing.T) {
	w := newWorld("s1")
	o := w.options()
	o.Supersede = "a0"
	res, err := Run(t.Context(), o)
	if h := w.attempts.history(); err != nil || res.Record.State != attempt.Bound || !strings.HasPrefix(h, "supersede/a0 admit") {
		t.Fatalf("takeover: %+v err=%v history=%s", res, err, h)
	}
	// A takeover refused for want of a slot is on record; the wait that
	// follows opens in the attempt's own name and is reported.
	full := newWorld("s1")
	full.attempts.supersedeErr = attempt.NoSlot{Endpoint: "endpoint:n1/h", Slots: 1}
	o = full.options()
	o.Supersede, o.SlotPoll = "a0", 5*time.Millisecond
	var waited []attempt.NoSlot
	o.Waiting = func(f attempt.NoSlot) { waited = append(waited, f) }
	res, err = Run(t.Context(), o)
	if h := full.attempts.history(); err != nil || res.Record.State != attempt.Bound || len(waited) != 1 || waited[0].Endpoint != "endpoint:n1/h" || !strings.HasPrefix(h, "supersede/a0 open admit") {
		t.Fatalf("takeover without a slot: %+v err=%v waited=%v history=%s", res, err, waited, h)
	}
	// A wait cut short fails the open on the context, workspace given back.
	cut, cancel := context.WithCancel(t.Context())
	defer cancel()
	stuck := newWorld("s1")
	stuck.attempts.openErr = attempt.NoSlot{Endpoint: "endpoint:n1/h", Slots: 1}
	o = stuck.options()
	o.SlotPoll = time.Hour
	o.Waiting = func(attempt.NoSlot) { cancel() }
	_, err = Run(cut, o)
	var step *StepError
	if !errors.As(err, &step) || step.Step != StepOpen || !errors.Is(err, context.Canceled) || stuck.workspaces.discarded != 1 {
		t.Fatalf("cut wait = %v discarded=%d", err, stuck.workspaces.discarded)
	}
}

func TestRunCommitsACompletionAsGiven(t *testing.T) {
	// Finish walks the record to bind-ready with a checkpoint of the work
	// on it: FinishCompletion commits the checkpoint, Complete the
	// completion the caller gave.
	for _, asGiven := range []bool{false, true} {
		w := newWorld("s1")
		o := w.options()
		o.Settlement.CommitAsGiven = asGiven
		o.Finish = func(ctx context.Context, e *Execution) (attempt.Completion, error) {
			if _, err := w.attempts.Advance(ctx, e.Record.ID, attempt.BindReady, "test", func(r *attempt.Record) {
				r.Result = &attempt.Result{Summary: "checkpoint"}
			}); err != nil {
				return attempt.Completion{}, err
			}
			return attempt.Completion{Result: attempt.Result{Summary: "candidate"}}, nil
		}
		res, err := Run(t.Context(), o)
		want := "checkpoint"
		if asGiven {
			want = "candidate"
		}
		if err != nil || res.Record.State != attempt.Bound || res.Record.Result.Summary != want || !res.Durable {
			t.Fatalf("asGiven=%v: %+v err=%v", asGiven, res.Record, err)
		}
		if h := w.attempts.history(); strings.Contains(h, "finish") == asGiven {
			t.Fatalf("asGiven=%v: history = %s", asGiven, h)
		}
	}
}

func TestRunFailsWhatFinishRefused(t *testing.T) {
	refused := errors.New("wrote outside its declared paths")
	refuse := func(context.Context, *Execution) (attempt.Completion, error) {
		return attempt.Completion{}, &Failure{Cause: refused}
	}
	w := newWorld("s1")
	o := w.options()
	o.Finish = refuse
	res, err := Run(t.Context(), o)
	if err != refused || res.Record.State != attempt.Failed || res.Record.Error != refused.Error() || !res.Durable || w.workspaces.discarded != 1 {
		t.Fatalf("hub refusal: %+v err=%v discarded=%d", res, err, w.workspaces.discarded)
	}
	// A node-owned session's refusal is recorded too, not left to an
	// observer, and cleanup follows the transition.
	m := newWorld("ns_1")
	m.roster.bindings = []ability.Binding{{Name: "tool"}}
	o = m.options()
	o.Settlement = Settlement{Quarantine: QuarantineManaged, DetachManaged: true, Detachment: DetachQuarantines}
	o.Finish = refuse
	res, err = Run(t.Context(), o)
	var detached *execution.RetainedObserverDetached
	if err != refused || errors.As(err, &detached) || res.Record.State != attempt.Failed || res.Unsettled || !res.Durable {
		t.Fatalf("managed refusal: %+v err=%v", res, err)
	}
	if want := "open admit prepared/test arm/test-open session running/test settled/test failed/test close release discard"; m.attempts.history() != want {
		t.Fatalf("managed refusal order = %s", m.attempts.history())
	}
}

func TestRunLeavesADeferredCompletionToTheObserverThatComesBack(t *testing.T) {
	waiting := errors.New("the verifier's own execution is retained")
	wait := func(context.Context, *Execution) (attempt.Completion, error) {
		return attempt.Completion{}, &Deferred{Cause: waiting}
	}
	m := newWorld("ns_1")
	m.roster.bindings = []ability.Binding{{Name: "tool"}}
	o := m.options()
	o.Settlement = Settlement{Quarantine: QuarantineManaged, DetachManaged: true, Detachment: DetachQuarantines}
	o.Finish = wait
	res, err := Run(t.Context(), o)
	var deferred *Deferred
	if !errors.As(err, &deferred) || deferred.Cause != waiting || res.Record.State != attempt.Running || res.Unsettled || res.Durable {
		t.Fatalf("deferred: %+v err=%v", res, err)
	}
	if h := m.attempts.history(); len(m.sessions.closed) != 0 || len(m.roster.released) != 0 || m.workspaces.discarded != 0 || !strings.HasSuffix(h, "settled/test") {
		t.Fatalf("a deferred completion gave something back: closed=%v released=%v discarded=%d history=%s", m.sessions.closed, m.roster.released, m.workspaces.discarded, h)
	}
	// Nobody comes back for a hub session: the attempt fails on the cause.
	h := newWorld("s1")
	o = h.options()
	o.Finish = wait
	res, err = Run(t.Context(), o)
	if err != waiting || res.Record.State != attempt.Failed || h.workspaces.discarded != 1 {
		t.Fatalf("hub deferral: %+v err=%v", res, err)
	}
}

func TestRunQuarantinesAHubCompletionThatEndsUnconfirmed(t *testing.T) {
	unconfirmed := errors.Join(harness.ErrStopUnconfirmed, errors.New("the verifier's exit was not seen"))
	unseen := func(context.Context, *Execution) (attempt.Completion, error) {
		return attempt.Completion{}, unconfirmed
	}
	w := newWorld("s1")
	o := w.options()
	o.Settlement.QuarantineFinish = true
	o.Finish = unseen
	res, err := Run(t.Context(), o)
	if !errors.Is(err, harness.ErrStopUnconfirmed) || !res.Unsettled || !res.Record.Unsettled || res.Record.State != attempt.Running || res.Durable || w.workspaces.discarded != 0 {
		t.Fatalf("unconfirmed completion was not quarantined: %+v err=%v discarded=%d", res, err, w.workspaces.discarded)
	}
	// Without it a hub completion's unconfirmed stop fails the attempt.
	plain := newWorld("s1")
	o = plain.options()
	o.Finish = unseen
	res, err = Run(t.Context(), o)
	if !errors.Is(err, harness.ErrStopUnconfirmed) || res.Unsettled || res.Record.State != attempt.Failed || plain.workspaces.discarded != 1 {
		t.Fatalf("unconfirmed completion without the rule: %+v err=%v", res, err)
	}
}
