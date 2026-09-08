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
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/roster"
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
	failAt  attempt.State
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
	if f.openErr != nil {
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
	return f.Advance(ctx, id, attempt.Bound, actor, func(r *attempt.Record) { r.Result = &completion.Result; r.Usage = completion.Usage })
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
	runner  *runner
	openErr error
	opened  int
	closed  []string
	log     func(string)
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
	return nil
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
	if want := "settled/test release finish bound/test close discard"; w.attempts.history() != want {
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
