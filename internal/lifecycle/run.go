package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/ability"
	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/roster"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/view"
)

// Attempts is the attempt ledger as Run drives it: one attempt opened,
// moved, settled and closed under its own leases.
type Attempts interface {
	Leaser
	Open(ctx context.Context, spec attempt.Spec) (attempt.Record, error)
	Get(ctx context.Context, id string) (attempt.Record, error)
	Advance(ctx context.Context, id string, to attempt.State, actor string, mutate func(*attempt.Record)) (attempt.Record, error)
	ArmSession(ctx context.Context, id, actor string) error
	MarkSessionSettled(ctx context.Context, id, actor string) error
	MarkUnsettled(ctx context.Context, id, actor string, cause error, usage *attempt.Usage) error
	FinishCompletion(ctx context.Context, id, actor string, completion attempt.Completion) (attempt.Record, error)
	RejectCompletion(ctx context.Context, id, actor string, completion attempt.Completion, cause error) error
	FailWith(ctx context.Context, id, actor, cause string, usage *attempt.Usage) (attempt.Record, error)
}

// Roster admits an attempt onto its machine and gives back what admission
// bound there.
type Roster interface {
	Admit(ctx context.Context, c roster.Candidate, requires, uses []string, attemptID string) (ability.Admission, []ability.Binding, error)
	Release(ctx context.Context, node, attemptID string)
}

// Sessions opens and closes sessions where they run.
type Sessions interface {
	Closer
	OpenSession(ctx context.Context, at harness.Placement, upstream, workdir string, servers []acp.MCPServer) (harness.Runner, error)
}

// Workspaces gives an attempt's workspace back once the attempt is over.
type Workspaces interface {
	Discard(ctx context.Context, ws project.Workspace) error
}

// Step is where in the lifecycle a run is when something goes wrong.
type Step int

const (
	// StepOpen is attempts.Open: no attempt exists yet.
	StepOpen Step = iota + 1
	// StepLeased is the caller's hook on the leased attempt (a budget).
	StepLeased
	// StepAdmit is roster.Admit.
	StepAdmit
	// StepPrepare is the caller's preparation and the move to Prepared.
	StepPrepare
	// StepArm is ArmSession.
	StepArm
	// StepSession is OpenSession and the cancellation check on its heels.
	StepSession
	// StepStart is the caller's arming, the move to Running, and what the
	// caller does once the attempt runs.
	StepStart
	// StepDrive is the prompt: a node-owned session whose observer this
	// process can no longer be reports its detachment here.
	StepDrive
	// StepSettle is MarkSessionSettled on a node-owned session.
	StepSettle
	// StepFinish is the caller's completion and the terminal transition.
	StepFinish
)

// StepError says which of Run's own steps failed; the work failing — the
// prompt, the caller's validation or completion — comes back as its own
// error. Callers put their own words on it; errors.Is and errors.As see
// through it.
type StepError struct {
	Step Step
	Err  error
}

func (e *StepError) Error() string { return e.Err.Error() }
func (e *StepError) Unwrap() error { return e.Err }

// Refused is an admission that did not admit the attempt.
type Refused struct{ Admission ability.Admission }

func (e *Refused) Error() string { return "admission refused: " + e.Admission.Unmet() }

// Rejected is a completion the caller could not finish assembling: the
// attempt closes with what there is, its workspace kept for review.
type Rejected struct {
	Completion attempt.Completion
	Cause      error
}

func (e *Rejected) Error() string { return e.Cause.Error() }
func (e *Rejected) Unwrap() error { return e.Cause }

// Quarantine says when a prompt that ended without settling — no final
// answer, no proof the process exited — is an unconfirmed stop.
type Quarantine int

const (
	// QuarantineNever: an unsettled prompt is a failure unless the
	// harness itself says the stop is unconfirmed (chat turns).
	QuarantineNever Quarantine = iota
	// QuarantineManaged: only a node-owned session's unsettled prompt is
	// unconfirmed; a hub session's is closed, and the close decides
	// (delegations).
	QuarantineManaged
	// QuarantineAlways: every unsettled prompt is an unconfirmed stop
	// (planning and verification).
	QuarantineAlways
)

// Detachment says whether a node-owned session's detached observer
// quarantines the record.
type Detachment int

const (
	// DetachSilently leaves the record as it is: the scope's unresolved
	// error is the only trace (planning and verification).
	DetachSilently Detachment = iota
	// DetachQuarantines marks the record unsettled (chat turns).
	DetachQuarantines
	// DetachQuarantinesUnlessCancelled marks it unless the run was
	// cancelled, so a recovered observer can still complete it
	// (delegations).
	DetachQuarantinesUnlessCancelled
)

// Settlement is how a caller reads the end of a prompt. Each field is a
// difference the callers had before Run existed, kept explicit here
// rather than unified.
type Settlement struct {
	// StoppedSettles: a process known to have exited settles the prompt
	// as much as the harness's final answer does.
	StoppedSettles bool
	// Quarantine is when an unsettled prompt is an unconfirmed stop.
	Quarantine Quarantine
	// DetachManaged: a node-owned session whose prompt did not settle, or
	// whose observer was cancelled, is left to the node and reported as a
	// detached observer instead of being failed.
	DetachManaged bool
	// Detachment is what a detached observer writes on the record.
	Detachment Detachment
	// QuarantineUnpublished: a node-owned session that was opened but
	// whose identity never reached the record is quarantined, not failed:
	// the node has a process this process cannot vouch for.
	QuarantineUnpublished bool
	// CancelDetaches: a cancelled run detaches from a node-owned session
	// even when its prompt settled (planning, verification, delegations);
	// without it a settled prompt is recorded on a detached context (chat
	// turns).
	CancelDetaches bool
	// RejectManaged: a node-owned session's completion that cannot be
	// committed is rejected, and a failure that cannot be recorded is
	// returned, as for a hub session (chat turns); without it either
	// detaches the observer and leaves the record for the one that comes
	// back to it (planning, verification, delegations).
	RejectManaged bool
	// KeepSession: once the caller has armed it, the session is the
	// conversation's and outlives the run; a failure before that closes it.
	KeepSession bool
	// CommitTimeout, when set, commits the completion on a context detached
	// from the run's cancellation and bounded by it; zero commits on the
	// run's own context, so a lost lease rejects the result.
	CommitTimeout time.Duration
}

// Options is what a caller settles before Run opens the attempt.
type Options struct {
	Attempts   Attempts
	Roster     Roster     // nil: no admission
	Sessions   Sessions   // nil with Open set: sessions are the caller's to open
	Workspaces Workspaces // nil: the workspace is not Run's to discard
	// Actor signs the ledger transitions.
	Actor string
	// Spec is the attempt to open.
	Spec attempt.Spec
	// SlotPoll retries an open refused for want of a slot every SlotPoll
	// until the context ends; zero refuses at once.
	SlotPoll time.Duration
	// Lost is told, after the run is cancelled, that the lease is gone.
	Lost func()

	// Candidate, Requires and Uses are what admission judges.
	Candidate      roster.Candidate
	Requires, Uses []string
	// AdmitUnsure lets an Unsure verdict run; a refusal never does.
	AdmitUnsure bool

	// ArmActor, when set, records that a session is about to be opened,
	// so a lost open reply is a pending open and not nothing; a rejected
	// open is disarmed by ArmActor-rejected.
	ArmActor string
	// At, Upstream, Workdir and Servers open the session; admission's
	// bindings follow Servers.
	At       harness.Placement
	Upstream string
	Workdir  string
	Servers  []acp.MCPServer
	// Open, when set, opens the session instead of Sessions.OpenSession.
	Open func(ctx context.Context, e *Execution) (harness.Runner, error)
	// Model and ModelOptions are pinned on a fresh session.
	Model        string
	ModelOptions map[string]string

	// Prompt and its companions are what Drive sends; TurnPrompt uses the
	// turn entry on every session, not only node-owned ones. Resume, for
	// Reattach, follows the command the node already accepted instead.
	Prompt     string
	Media      []harness.Media
	TurnPrompt bool
	Resume     bool
	// Replay, for Reattach, is an outcome the caller recovered from the
	// record: settled without a prompt, its spend the record's.
	Replay  *Outcome
	Ask     permission.AskFunc
	AskUser acphost.AskUserFunc
	Observe func(view.Progress)

	// Leased runs on the leased attempt, before admission; the context it
	// returns is the run's from then on.
	Leased func(ctx context.Context, e *Execution) (context.Context, error)
	// Prepare runs after admission; its mutation is written with the move
	// to Prepared.
	Prepare func(ctx context.Context, e *Execution) (func(*attempt.Record), error)
	// Arm runs on the open session; its mutation is written with the move
	// to Running.
	Arm func(ctx context.Context, e *Execution) (func(*attempt.Record), error)
	// Started runs once the attempt is running, before the prompt.
	Started func(ctx context.Context, e *Execution) error
	// Ended is told that the prompt has ended, however it ended, before
	// settlement.
	Ended func(e *Execution)
	// Validate judges an answer the prompt ended well with; its error is
	// what the attempt failed on.
	Validate func(e *Execution) error
	// Finish turns a prompt that ended well into the attempt's completion.
	// A *Rejected error closes the attempt with a partial completion; on a
	// node-owned session any other error detaches the observer.
	Finish func(ctx context.Context, e *Execution) (attempt.Completion, error)
	// Failed adds a result to a failure's record; on a node-owned session
	// an error here detaches the observer.
	Failed func(e *Execution, cause error) (*attempt.Result, error)
	// Wrap puts the caller's words on a failed step before it is recorded
	// and returned; without it the step's own error is recorded and the
	// caller reads the StepError afterwards.
	Wrap func(step Step, e *Execution, err error) error

	Settlement Settlement
}

// Execution is one attempt on its way through Run; hooks see and amend it.
type Execution struct {
	o Options
	// Record is the attempt as last written or read.
	Record attempt.Record
	// Admission and Bindings are what admission decided and bound.
	Admission ability.Admission
	Bindings  []ability.Binding
	// Upstream is the session to resume; a hook that opens fresh clears it.
	Upstream string
	// Prompt and Servers start as the options' and may be amended before
	// the session opens.
	Prompt  string
	Servers []acp.MCPServer
	// Session is the open session; Managed says the node owns it.
	Session harness.Runner
	Managed bool
	// Outcome is how the prompt ended; Usage what it cost.
	Outcome Outcome
	Usage   *attempt.Usage

	stopBeat func()
	// armed says the caller's arming succeeded: with KeepSession the
	// session is the conversation's from then on.
	armed bool
	// bound says the record's admission bound something on the machine,
	// for a reattached execution whose bindings this process never saw.
	bound     bool
	driven    bool
	unsettled bool
	durable   bool
	cleanup   error
}

// Result is what Run hands back.
type Result struct {
	Record   attempt.Record
	Session  harness.Runner
	Managed  bool
	Answer   string
	Activity []string
	Usage    *attempt.Usage
	// Last is the harness's last report, which Usage was read from.
	Last view.Progress
	// Driven says the prompt was sent; Settled that it ended by evidence.
	Driven  bool
	Settled bool
	// Unsettled says the record was quarantined.
	Unsettled bool
	// Durable says the attempt reached a terminal state and its cleanup
	// — session, bindings, workspace — succeeded.
	Durable bool
	// CleanupErr is what failed after the terminal transition.
	CleanupErr error
}

// Run takes one attempt from open to closed: Open → Keep → Admit →
// Prepared → ArmSession → OpenSession → preferences → Running → Drive →
// settle → the caller's completion → FinishCompletion, FailWith or
// MarkUnsettled → Close, Release, Discard. Callers choose what they
// prepare, what they do with the answer, and how the end is read.
func Run(ctx context.Context, o Options) (Result, error) {
	e := &Execution{o: o, Upstream: o.Upstream, Prompt: o.Prompt, Servers: o.Servers, stopBeat: func() {}}
	err := e.run(ctx)
	return e.result(), err
}

// Reattach joins an execution already in flight from the prompt on: a
// node-owned session this process opened and recorded, or found again by
// its receipt. Drive, settle, the caller's completion, the terminal
// transition and cleanup are Run's; opening, admission and the session
// are the caller's, done or recovered before.
func Reattach(ctx context.Context, o Options, record attempt.Record, session harness.Runner) (Result, error) {
	e := &Execution{o: o, Record: record, Session: session, Managed: Managed(session), Prompt: o.Prompt, Servers: o.Servers, stopBeat: func() {}, armed: true}
	e.bound = record.Admission != nil && len(record.Admission.Bound) > 0
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	e.stopBeat = Keep(ctx, o.Attempts, record.ID, func() {
		stop()
		if o.Lost != nil {
			o.Lost()
		}
	})
	defer e.stopBeat()
	var err error
	if o.Replay != nil {
		e.Outcome, e.Usage, e.driven = *o.Replay, record.Usage, true
		err = e.read(ctx)
	} else {
		err = e.drive(ctx)
	}
	err = e.close(ctx, err)
	return e.result(), err
}

// Armed says the caller's arming succeeded, so a later failure is the
// attempt's, not the session's.
func (e *Execution) Armed() bool { return e.armed }

func (e *Execution) result() Result {
	return Result{Record: e.Record, Session: e.Session, Managed: e.Managed, Answer: e.Outcome.Answer, Activity: e.Outcome.Activity, Usage: e.Usage, Last: e.Outcome.Last,
		Driven: e.driven, Settled: e.settled(), Unsettled: e.unsettled, Durable: e.durable, CleanupErr: e.cleanup}
}

func (e *Execution) run(ctx context.Context) error {
	o := e.o
	if err := e.open(ctx); err != nil {
		e.discard(ctx)
		return e.step(StepOpen, err)
	}
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	e.stopBeat = Keep(ctx, o.Attempts, e.Record.ID, func() {
		stop()
		if o.Lost != nil {
			o.Lost()
		}
	})
	defer e.stopBeat()
	if o.Leased != nil {
		next, err := o.Leased(ctx, e)
		if err != nil {
			return e.close(ctx, e.step(StepLeased, err))
		}
		ctx = next
	}
	err := e.start(ctx)
	if err == nil {
		err = e.drive(ctx)
	}
	return e.close(ctx, err)
}

// open leases the attempt, waiting for a slot when asked to.
func (e *Execution) open(ctx context.Context) error {
	for {
		record, err := e.o.Attempts.Open(ctx, e.o.Spec)
		var full attempt.NoSlot
		if err == nil || e.o.SlotPoll <= 0 || !errors.As(err, &full) {
			e.Record = record
			return err
		}
		timer := time.NewTimer(e.o.SlotPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// start takes the leased attempt to running with a session open on it.
func (e *Execution) start(ctx context.Context) error {
	o := e.o
	id := e.Record.ID
	if o.Roster != nil {
		admission, bindings, err := o.Roster.Admit(ctx, o.Candidate, o.Requires, o.Uses, id)
		if err != nil {
			return e.step(StepAdmit, err)
		}
		if admission.Refused() || (!o.AdmitUnsure && !admission.OK()) {
			return e.step(StepAdmit, &Refused{admission})
		}
		e.Admission, e.Bindings = admission, bindings
	}
	var prepare func(*attempt.Record)
	if o.Prepare != nil {
		mutate, err := o.Prepare(ctx, e)
		if err != nil {
			return e.step(StepPrepare, err)
		}
		prepare = mutate
	}
	admission := e.Admission
	prepared, err := o.Attempts.Advance(ctx, id, attempt.Prepared, o.Actor, func(r *attempt.Record) {
		if o.Roster != nil {
			r.Admission = &admission
		}
		if prepare != nil {
			prepare(r)
		}
	})
	if err != nil {
		return e.step(StepPrepare, err)
	}
	e.Record = prepared
	if o.ArmActor != "" {
		if err := o.Attempts.ArmSession(ctx, id, o.ArmActor); err != nil {
			return e.step(StepArm, err)
		}
	}
	if err := e.openSession(ctx); err != nil {
		return e.step(StepSession, err)
	}
	var arm func(*attempt.Record)
	if o.Arm != nil {
		mutate, err := o.Arm(ctx, e)
		if err != nil {
			return e.step(StepStart, err)
		}
		arm = mutate
	}
	e.armed = true
	session := e.Session
	running, err := o.Attempts.Advance(ctx, id, attempt.Running, o.Actor, func(r *attempt.Record) {
		r.Session = session.ID()
		if arm != nil {
			arm(r)
		}
	})
	if err != nil {
		return e.step(StepStart, err)
	}
	e.Record = running
	if o.Started != nil {
		if err := o.Started(ctx, e); err != nil {
			return e.step(StepStart, err)
		}
	}
	return nil
}

// openSession opens the session with the caller's servers and admission's
// bindings, and pins the agent's preferences on a fresh one. A rejected
// open on an armed attempt is disarmed; an unconfirmed one is not.
func (e *Execution) openSession(ctx context.Context) error {
	o := e.o
	servers := append(append([]acp.MCPServer(nil), e.Servers...), roster.ToMCP(e.Bindings)...)
	var session harness.Runner
	var err error
	if o.Open != nil {
		e.Servers = servers
		session, err = o.Open(ctx, e)
	} else {
		session, err = o.Sessions.OpenSession(ctx, o.At, e.Upstream, o.Workdir, servers)
	}
	if err != nil {
		if o.ArmActor != "" && !errors.Is(err, harness.ErrStopUnconfirmed) {
			if markerErr := o.Attempts.MarkSessionSettled(ctx, e.Record.ID, o.ArmActor+"-rejected"); markerErr != nil {
				return errors.Join(err, markerErr)
			}
		}
		return err
	}
	e.Session, e.Managed = session, Managed(session)
	if err := ctx.Err(); err != nil {
		return err
	}
	if e.Upstream == "" {
		harness.ApplyPreferences(ctx, session, o.Spec.Agent, o.Model, o.ModelOptions)
	}
	return nil
}

// drive sends the prompt and reads how it ended.
func (e *Execution) drive(ctx context.Context) error {
	o := e.o
	e.driven = true
	e.Outcome = Drive{Session: e.Session, Prompt: e.Prompt, Media: o.Media, Turn: o.TurnPrompt || e.Managed, Resume: o.Resume, Ask: o.Ask, AskUser: o.AskUser, Observe: o.Observe}.Run(ctx)
	e.Usage = Usage(e.Outcome.Last)
	return e.read(ctx)
}

// read is how the prompt's end reads under the caller's rules.
func (e *Execution) read(ctx context.Context) error {
	o := e.o
	if o.Ended != nil {
		o.Ended(e)
	}
	err := e.Outcome.Err
	if !e.settled() && (o.Settlement.Quarantine == QuarantineAlways || o.Settlement.Quarantine == QuarantineManaged && e.Managed) {
		err = errors.Join(err, harness.ErrStopUnconfirmed)
	}
	if err == nil && ctx.Err() != nil && !(e.Managed && o.Settlement.DetachManaged) {
		// A cancelled run's settled prompt is recorded as cancelled, before
		// validation, as it always was. A node-owned session's is
		// closeManaged's to read: an explicit stop is a cancelled turn, a
		// cancelled observer detaches or, for a retained chat turn,
		// commits what the node finished.
		err = ctx.Err()
	}
	if err == nil && o.Validate != nil {
		err = o.Validate(e)
	}
	return err
}

// settled is whether the prompt is over by the evidence this caller
// accepts.
func (e *Execution) settled() bool {
	if !e.driven {
		return e.o.Settlement.StoppedSettles && e.Session != nil && Stopped(e.Session)
	}
	return e.Outcome.PromptSettled || (e.o.Settlement.StoppedSettles && Stopped(e.Session))
}

// close records how the run ended and gives back what it held. Each
// step that runs after the caller's hooks — the terminal transition, the
// quarantine, giving the bindings and the workspace back — is bounded on
// its own: the hooks may take longer than one cleanup is allowed, and a
// context minted before them would be gone when they return.
func (e *Execution) close(ctx context.Context, err error) error {
	if e.Record.ID == "" {
		return err
	}
	o := e.o
	if e.Managed && e.Record.Session == "" && (errors.Is(err, harness.ErrStopUnconfirmed) || o.Settlement.QuarantineUnpublished) {
		// The node has a process whose identity never reached the record:
		// not this process's to close or to call stopped.
		return e.quarantine(ctx, errors.Join(harness.ErrStopUnconfirmed, err))
	}
	if e.Managed && e.driven && o.Settlement.DetachManaged {
		return e.closeManaged(ctx, err)
	}
	id := e.Record.ID
	cleanup, stop := Cleanup(ctx)
	defer stop()
	switch {
	case e.Session == nil || errors.Is(err, harness.ErrStopUnconfirmed):
	case o.Settlement.KeepSession && e.armed:
		// The conversation's session outlives the run.
	case o.Settlement.KeepSession:
		// A session the conversation never took wrote nothing: tear it
		// down as well as it can be, and the failure stands on its own.
		if closeErr := o.Sessions.CloseSession(cleanup, o.At, e.Session.ID()); closeErr != nil {
			e.Session.Abort()
		}
	default:
		if closeErr := Close(cleanup, o.Sessions, o.At, e.Session); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}
	if errors.Is(err, harness.ErrStopUnconfirmed) {
		return e.quarantine(ctx, err)
	}
	if e.Session != nil && e.settled() {
		if markErr := o.Attempts.MarkSessionSettled(cleanup, id, o.Actor); markErr != nil {
			err = errors.Join(err, fmt.Errorf("record prompt settlement: %w", markErr))
		}
	}
	e.release(ctx)
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		return e.fail(ctx, err)
	}
	return e.finish(ctx)
}

// closeManaged settles a node-owned session the node keeps: the observer
// detaches when it cannot vouch for the end, and cleans up — session,
// bindings, workspace — only after a terminal transition it made itself.
func (e *Execution) closeManaged(ctx context.Context, err error) error {
	o := e.o
	id := e.Record.ID
	settled := e.settled()
	if settled && errors.Is(execution.CheckExecution(ctx), task.ErrExecutionStopped) {
		// An explicit stop of a settled prompt is a fact to record, on a
		// context the stop itself did not cancel.
		var stop context.CancelFunc
		ctx, stop = Cleanup(ctx)
		defer stop()
		if err == nil {
			err = harness.ErrTurnCanceled
		}
	}
	cancelled := ctx.Err() != nil
	quarantines := o.Settlement.Quarantine == QuarantineAlways || o.Settlement.Quarantine == QuarantineManaged
	if errors.Is(err, harness.ErrStopUnconfirmed) || !settled && (quarantines || cancelled) || cancelled && o.Settlement.CancelDetaches {
		return e.detach(ctx, StepDrive, errors.Join(err, ctx.Err()), cancelled)
	}
	if o.Settlement.CommitTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), o.Settlement.CommitTimeout)
		defer cancel()
	}
	if settled {
		if markErr := o.Attempts.MarkSessionSettled(ctx, id, o.Actor); markErr != nil {
			return e.detach(ctx, StepSettle, markErr, false)
		}
	}
	if err != nil {
		terminal, transition := e.failure(ctx, err)
		if transition != nil {
			if o.Settlement.RejectManaged {
				return errors.Join(err, fmt.Errorf("settle %s: %w", id, transition))
			}
			return e.detach(ctx, StepFinish, transition, false)
		}
		e.Record = terminal
	} else if err = e.commitManaged(ctx); err != nil {
		return err
	}
	// The bindings are the node's until the transition is on the record:
	// an observer that detaches before it leaves them for the one that
	// comes back.
	e.durable = true
	if e.Session != nil && !o.Settlement.KeepSession {
		if closeErr := Close(ctx, o.Sessions, o.At, e.Session); closeErr != nil {
			// The record is terminal, but its writer is not known to have
			// exited: the workspace, the slot and the bindings stay until
			// someone confirms the stop, and the record says so.
			e.cleanup, e.durable = errors.Join(e.cleanup, fmt.Errorf("close settled session: %w", closeErr)), false
			e.quarantine(ctx, closeErr)
			return err
		}
	}
	e.release(ctx)
	e.discard(ctx)
	return err
}

// commitManaged commits a node-owned session's completion; what cannot be
// committed is left for a returning observer, or rejected when the
// caller closes its own.
func (e *Execution) commitManaged(ctx context.Context) error {
	o := e.o
	var completion attempt.Completion
	if o.Finish != nil {
		var err error
		if completion, err = o.Finish(ctx, e); err != nil {
			var rejected *Rejected
			if o.Settlement.RejectManaged && errors.As(err, &rejected) {
				return e.reject(ctx, rejected.Completion, rejected.Cause)
			}
			if o.Settlement.RejectManaged {
				return e.fail(ctx, err)
			}
			return e.detach(ctx, StepFinish, err, false)
		}
	}
	if completion.Usage == nil {
		completion.Usage = e.Usage
	}
	e.stopBeat()
	completed, err := o.Attempts.FinishCompletion(ctx, e.Record.ID, o.Actor, completion)
	if err != nil {
		if o.Settlement.RejectManaged {
			return e.reject(ctx, completion, err)
		}
		return e.detach(ctx, StepFinish, err, false)
	}
	e.Record = completed
	return nil
}

// detach reports a node-owned session this process can no longer observe.
// A quarantine that could not be written is part of what it reports.
func (e *Execution) detach(ctx context.Context, step Step, cause error, cancelled bool) error {
	o := e.o
	cause = errors.Join(harness.ErrStopUnconfirmed, cause)
	switch o.Settlement.Detachment {
	case DetachQuarantines:
		cause = e.quarantine(ctx, cause)
	case DetachQuarantinesUnlessCancelled:
		if !cancelled {
			cause = e.quarantine(ctx, cause)
		}
	}
	return &StepError{step, &execution.RetainedObserverDetached{AttemptID: e.Record.ID, NodeID: e.Record.Node, SessionID: e.Record.Session, Cause: cause}}
}

// quarantine marks an attempt whose writer may still be running: the
// workspace, the slot and the bindings stay until someone confirms the
// stop. What comes back is the cause, with the marker's failure when it
// could not be written.
func (e *Execution) quarantine(ctx context.Context, cause error) error {
	cleanup, stop := Cleanup(ctx)
	defer stop()
	o := e.o
	e.unsettled = true
	if err := o.Attempts.MarkUnsettled(cleanup, e.Record.ID, o.Actor, cause, e.Usage); err != nil {
		cause = errors.Join(cause, err)
	}
	e.refresh(cleanup)
	return cause
}

func (e *Execution) refresh(ctx context.Context) {
	if record, err := e.o.Attempts.Get(ctx, e.Record.ID); err == nil {
		e.Record = record
	}
}

func (e *Execution) release(ctx context.Context) {
	if (len(e.Bindings) > 0 || e.bound) && e.o.Roster != nil {
		cleanup, stop := Cleanup(ctx)
		defer stop()
		e.o.Roster.Release(cleanup, e.o.Spec.Node, e.Record.ID)
	}
}

func (e *Execution) discard(ctx context.Context) {
	if e.o.Workspaces == nil {
		return
	}
	cleanup, stop := Cleanup(ctx)
	defer stop()
	if err := e.o.Workspaces.Discard(cleanup, e.o.Spec.Workspace); err != nil {
		e.cleanup, e.durable = errors.Join(e.cleanup, fmt.Errorf("discard %s: %w", e.o.Spec.ID, err)), false
	}
}

// failure records a failed attempt with what it cost and, when the caller
// has one, the result it produced anyway. The lease is renewed until the
// caller's hook has returned: the terminal transition is the last thing
// the lease fences, and the hook may outlive what the lease has left.
func (e *Execution) failure(ctx context.Context, cause error) (attempt.Record, error) {
	o := e.o
	var result *attempt.Result
	if o.Failed != nil {
		var err error
		if result, err = o.Failed(e, cause); err != nil {
			return attempt.Record{}, err
		}
	}
	e.stopBeat()
	if result == nil {
		return o.Attempts.FailWith(ctx, e.Record.ID, o.Actor, cause.Error(), e.Usage)
	}
	usage := e.Usage
	return o.Attempts.Advance(ctx, e.Record.ID, attempt.Failed, o.Actor, func(r *attempt.Record) {
		r.Error = cause.Error()
		if usage != nil {
			r.Usage = usage
		}
		r.Result = result
	})
}

// fail closes a hub-owned attempt as failed, on a cleanup context of its
// own, and gives its workspace back.
func (e *Execution) fail(ctx context.Context, cause error) error {
	cleanup, stop := Cleanup(ctx)
	defer stop()
	failed, err := e.failure(cleanup, cause)
	if err != nil {
		return errors.Join(cause, fmt.Errorf("settle %s: %w", e.Record.ID, err))
	}
	e.Record = failed
	e.durable = true
	e.discard(ctx)
	return cause
}

// finish commits the caller's completion; a rejected completion keeps the
// workspace for review. The lease is renewed through the caller's hook
// and stops just before the terminal transition it fences.
func (e *Execution) finish(ctx context.Context) error {
	o := e.o
	id := e.Record.ID
	commit := ctx
	if o.Settlement.CommitTimeout > 0 {
		var cancel context.CancelFunc
		commit, cancel = context.WithTimeout(context.WithoutCancel(ctx), o.Settlement.CommitTimeout)
		defer cancel()
	}
	var completion attempt.Completion
	if o.Finish != nil {
		var err error
		if completion, err = o.Finish(commit, e); err != nil {
			var rejected *Rejected
			if errors.As(err, &rejected) {
				return e.reject(ctx, rejected.Completion, rejected.Cause)
			}
			return e.fail(ctx, err)
		}
	}
	if completion.Usage == nil {
		completion.Usage = e.Usage
	}
	e.stopBeat()
	completed, err := o.Attempts.FinishCompletion(commit, id, o.Actor, completion)
	if err != nil {
		return e.reject(ctx, completion, err)
	}
	e.Record = completed
	e.durable = true
	e.discard(ctx)
	return nil
}

// reject closes the attempt on a completion that could not be committed;
// like every terminal transition, it is the first thing after the lease
// stops being renewed.
func (e *Execution) reject(ctx context.Context, completion attempt.Completion, cause error) error {
	cleanup, stop := Cleanup(ctx)
	defer stop()
	e.stopBeat()
	err := e.o.Attempts.RejectCompletion(cleanup, e.Record.ID, e.o.Actor, completion, cause)
	e.refresh(cleanup)
	return e.step(StepFinish, err)
}

// step is a failed step in the caller's words, when it has any.
func (e *Execution) step(step Step, err error) error {
	if e.o.Wrap != nil {
		err = e.o.Wrap(step, e, err)
	}
	return &StepError{step, err}
}
