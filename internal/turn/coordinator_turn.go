package turn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/agent"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/home"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/lifecycle"
	"github.com/gopact-ai/steve/internal/onboard"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/protocol"
	"github.com/gopact-ai/steve/internal/roster"
	"github.com/gopact-ai/steve/internal/state"
	"github.com/gopact-ai/steve/internal/view"
)

// chatTurn is one prompt's side of the lifecycle: the conversation's
// session it opens or resumes, the prompt it composes, the state it saves,
// and the after-snapshot its completion binds.
type chatTurn struct {
	c        *Coordinator
	req      Request
	selected agent.Agent
	clock    *turnClock
	spent    *turnSpend
	scope    *execution.Scope
	tracked  string
	prompt   string

	binding        project.Binding
	workspace      project.Workspace
	saved          state.Session
	capabilities   capability.Capabilities
	contextChanged bool

	// session is the conversation's record of the open session; managed
	// says a node owns it, or that its open may still be pending there.
	session  state.Session
	managed  bool
	building bool
	injected *Injected
	// finished says the completion was being assembled when the run
	// ended: the session state is already what the turn made it.
	finished bool
	pending  *project.Project
	result   Result
	run      lifecycle.Result
}

// turnSpec is the attempt a chat turn leases on the project's canonical
// workspace. The machine must qualify for the project's level; the roster
// knows both the machine's level and the endpoint's session cap.
func (c *Coordinator) turnSpec(ctx context.Context, req Request, selected agent.Agent, taskID string, binding project.Binding, workspace project.Workspace) (attempt.Spec, roster.Candidate, error) {
	if c.attempts == nil {
		return attempt.Spec{}, roster.Candidate{}, errors.New("turn: attempts are not wired")
	}
	spec := attempt.Spec{Execution: execution.Token(ctx),
		TaskID: taskID, TurnID: req.MessageID, Kind: attempt.KindChat, Project: binding.ProjectID,
		Node: selected.Node, Harness: selected.Harness, Agent: selected.ID,
		Workspace: workspace, Scope: attempt.ScopeUnrestricted, By: req.SenderOpenID,
	}
	if workspace.Kind == project.KindWorktree {
		spec.Scope = attempt.ScopePathSet
		spec.Base = workspace.Base
	}
	var cand roster.Candidate
	if c.fleet != nil {
		cand = c.fleet.ForAgent(ctx, selected)
		spec.Slots = cand.Slots
		spec.Region = cand.Region
		if p, ok, perr := c.projects.Get(ctx, binding.ProjectID); perr == nil && ok {
			spec.CanonicalRegion = c.fleet.RegionOf(p.Home.Node)
			if !p.Level.OrDefault().Admits(cand.Level.OrDefault()) {
				return attempt.Spec{}, roster.Candidate{}, UserError{Text: c.text.T(i18n.ProjectLevel, p.ID, p.Level.OrDefault(), selected.ID, placeLabel(selected.Node), cand.Level.OrDefault(), protocol.CommandProject)}
			}
		}
	}
	spec.Requires = selected.Requires
	return spec, cand, nil
}

func (t *chatTurn) options(spec attempt.Spec, candidate roster.Candidate) lifecycle.Options {
	spec.PluginRuntime = t.saved.PluginRuntime.Clone()
	c, req, selected := t.c, t.req, t.selected
	var fleet lifecycle.Roster
	if c.fleet != nil {
		fleet = c.fleet
	}
	return lifecycle.Options{
		Attempts: c.attempts, Roster: fleet, Sessions: c.runtime, Actor: "turn",
		Spec: spec,
		// A lost lease cancels the turn, because nothing done after it
		// could be recorded.
		Lost: func() {
			slog.Warn(fmt.Sprintf("turn: attempt %s lost its lease; cancelling the turn", spec.ID), "attempt", spec.ID, "task", t.tracked, "conversation", t.req.ConversationID)
		},
		// A chat turn is admitted like any other attempt: the machine's
		// own word on the agent's requirements, taken now, kept on the
		// record. A refusal ends the turn before a session is opened.
		Candidate: candidate, Requires: selected.Requires, Uses: selected.MCPServers, AdmitUnsure: true,
		At: placement(selected), Upstream: t.saved.UpstreamID, Workdir: t.workspace.Path, Servers: t.capabilities.MCPServers,
		Open:  t.open,
		Media: req.Images, TurnPrompt: true, Ask: req.OnAsk, AskUser: req.OnAskUser, Observe: req.OnProgress,
		Leased: t.leased, Prepare: t.prepare, Arm: t.arm, Started: t.started, Ended: t.ended, Finish: t.finish, Wrap: t.wrap,
		// The session is the conversation's once its state is saved; an
		// unsettled prompt is a failure unless the harness calls the stop
		// unconfirmed; the after-snapshot outlives the turn's own clock.
		Settlement: lifecycle.Settlement{KeepSession: true, CommitTimeout: 2 * time.Minute},
	}
}

// leased binds the attempt to the task's accounting.
func (t *chatTurn) leased(ctx context.Context, e *lifecycle.Execution) (context.Context, error) {
	c := t.c
	t.clock.mark("lease")
	if t.scope != nil {
		t.scope.SetAttempt(e.Record.ID)
	}
	if c.tasks != nil && e.Record.Execution != nil {
		if bindErr := c.tasks.BindAttempt(*e.Record.Execution, e.Record.ID, e.Record.TurnID); bindErr != nil {
			return ctx, fmt.Errorf("bind task accounting: %w", bindErr)
		}
	}
	return ctx, nil
}

// prepare takes the before-snapshot, the precondition of running in
// place: what the turn changes is measured against it.
func (t *chatTurn) prepare(ctx context.Context, e *lifecycle.Execution) (func(*attempt.Record), error) {
	c, req, workspace := t.c, t.req, t.workspace
	t.clock.mark("admit")
	if req.OnTurnReady != nil {
		req.OnTurnReady(t.tracked, e.Record.ID)
	}
	defer t.clock.mark("before")
	if workspace.Kind == project.KindWorktree {
		base := workspace.Base
		return func(r *attempt.Record) { r.Base = base }, nil
	}
	if c.artifacts == nil {
		return nil, nil
	}
	p, ok, perr := c.projects.Get(ctx, t.binding.ProjectID)
	if perr != nil || !ok {
		return nil, nil
	}
	before, _, serr := c.snapshot(ctx, p, workspace, "", e.Record.ID, "before turn "+req.MessageID)
	if serr != nil {
		return nil, fmt.Errorf("before-snapshot: %w", serr)
	}
	return func(r *attempt.Record) { r.Base = before.ID }, nil
}

// open reopens the conversation's session, or starts a fresh one in this
// same turn when the saved upstream cannot be reopened, instead of failing
// once and waiting for the user to send again.
func (t *chatTurn) open(ctx context.Context, e *lifecycle.Execution) (harness.Runner, error) {
	c, selected := t.c, t.selected
	runner, err := c.open(ctx, t.saved, selected, t.workspace.Path, e.Servers)
	if err != nil && t.saved.UpstreamID != "" && !strings.HasPrefix(t.saved.UpstreamID, "ns_") && !errors.Is(err, harness.ErrNodeSessionUnavailable) {
		if stateErr := c.store.DeleteSession(t.req.ConversationID, selected.ID); stateErr != nil {
			slog.Error(fmt.Sprintf("turn: delete unreopenable session state: %v", stateErr), "attempt", e.Record.ID, "conversation", t.req.ConversationID, "agent", selected.ID)
		}
		t.saved.UpstreamID = ""
		t.saved.InstructionsApplied = false
		e.Upstream = ""
		runner, err = c.open(ctx, t.saved, selected, t.workspace.Path, e.Servers)
	}
	t.clock.mark("session")
	return runner, err
}

// arm makes the open session the conversation's: saved, tainted until the
// prompt ends well, and reachable for /cancel.
func (t *chatTurn) arm(_ context.Context, e *lifecycle.Execution) (func(*attempt.Record), error) {
	c, req, selected := t.c, t.req, t.selected
	runner := e.Session
	t.managed = e.Managed
	req.phase(view.PhaseRunning)
	t.session = state.Session{
		PluginRuntime:  e.Record.PluginRuntime.Clone(),
		ConversationID: req.ConversationID, AgentID: selected.ID, HarnessID: selected.Harness,
		NodeID:     selected.Node,
		UpstreamID: runner.ID(), Workspace: t.workspace.Path, CapabilityHash: t.capabilities.Fingerprint,
		ProjectID: t.binding.ProjectID, ProjectVersion: t.binding.Version,
		SessionConfigHash:   t.capabilities.SessionFingerprint,
		InstructionsApplied: t.saved.InstructionsApplied, Tainted: true,
		AgentToken: t.saved.AgentToken,
	}
	if e.Record.PluginRuntime != nil {
		t.session.PluginSkillsFingerprint = t.capabilities.SkillsFingerprint
	}
	if err := c.store.SaveSession(t.session); err != nil {
		return nil, err
	}
	c.setRunner(req.ConversationID, selected.ID, runner)
	preferences := sessionPreferences(runner)
	return func(r *attempt.Record) { r.Preferences = preferences }, nil
}

// started composes what the agent is given and binds its tools to the
// running attempt.
func (t *chatTurn) started(ctx context.Context, e *lifecycle.Execution) error {
	c, req := t.c, t.req
	if err := t.compose(e); err != nil {
		return err
	}
	if err := c.bindExecutionGate(ctx, req.ConversationID, e.Record.ID); err != nil {
		return err
	}
	t.clock.mark("arm")
	return nil
}

// compose is the prompt with the speaker, the listening prefix, the
// onboarding continuation and the assembled instructions in front of it,
// kept on the record of what the agent saw.
func (t *chatTurn) compose(e *lifecycle.Execution) error {
	c, req, selected, capabilities := t.c, t.req, t.selected, t.capabilities
	user := t.prompt
	if req.SenderOpenID != "" || req.ChatType != "" {
		speaker := req.SenderOpenID
		if speaker == "" {
			speaker = "-"
		}
		ownerFlag := "false"
		if req.SenderOpenID != "" && req.SenderOpenID == c.ownerOpenID {
			ownerFlag = "true"
		}
		user = fmt.Sprintf("[steve: speaker=%s owner=%s chat=%s]\n%s", speaker, ownerFlag, req.ChatType, t.prompt)
	}
	if prefix := c.listenPrefix(req); prefix != "" {
		user = prefix + user
	}
	if t.binding.ProjectID == c.homeProject {
		building, err := c.buildingProfile(req)
		if err != nil {
			return err
		}
		t.building = building
	}
	if t.building {
		if _, shared := c.home.(home.IdentityEditor); shared {
			user = onboard.ContinueShared(c.text.Locale()) + "\n\n" + user
		} else {
			user = onboard.Continue(c.text.Locale(), c.homePath) + "\n\n" + user
		}
	}
	injected := &Injected{
		Project: t.binding.ProjectID, Workspace: t.workspace.Path,
		Agent: selected.ID, Node: selected.Node, Harness: selected.Harness, Model: selected.Model, Options: selected.Options,
		Session: e.Session.ID(), NewSession: t.saved.UpstreamID == "", Fingerprint: capabilities.Fingerprint,
		InstructionsBytes: len(capabilities.Instructions), Prompt: user,
	}
	for _, srv := range e.Servers {
		injected.MCPServers = append(injected.MCPServers, srv.Name)
	}
	if !t.session.InstructionsApplied && (capabilities.Instructions != "" || t.contextChanged) {
		instructions := capabilities.Instructions
		if t.contextChanged {
			instructions = "[steve: context update]\nThe following is the current context. It replaces the previously supplied Steve context, identity and profile. Keep the conversation history and continue with the user's message below.\n\n" + instructions
		}
		user = instructions + "\n\n" + user
		injected.InstructionsSent = true
		injected.Instructions = instructions
		injected.InstructionsBytes = len(instructions)
	}
	t.injected = injected
	e.Prompt = user
	return nil
}

func (t *chatTurn) ended(e *lifecycle.Execution) {
	t.clock.mark("prompt")
	if e.Outcome.PromptSettled {
		t.req.phase(view.PhaseFinishing)
	}
}

// finish saves the session as the turn left it, applies a generated
// identity, and assembles the completion around the after-snapshot.
func (t *chatTurn) finish(ctx context.Context, e *lifecycle.Execution) (attempt.Completion, error) {
	c, selected := t.c, t.selected
	t.finished = true
	t.clock.mark("settle")
	out := e.Outcome.Answer
	t.session.Tainted = false
	t.session.InstructionsApplied = true
	if err := c.store.SaveSession(t.session); err != nil {
		slog.Error(fmt.Sprintf("turn: save completed session state: %v", err), "attempt", e.Record.ID, "conversation", t.req.ConversationID, "agent", selected.ID)
		out += "\n\n" + c.text.T(i18n.StateSaveFailed, protocol.CommandNew)
	}
	t.clock.mark("save")
	if t.building {
		var reply string
		var applyErr error
		if editor, shared := c.home.(home.IdentityEditor); shared {
			reply, _, applyErr = onboard.ApplyWith(out, func(soul, user string) error { return editor.WriteIdentity(ctx, soul, user) })
		} else {
			reply, _, applyErr = onboard.Apply(c.homePath, out)
		}
		if applyErr != nil {
			return attempt.Completion{}, fmt.Errorf("save generated identity: %w", applyErr)
		}
		out = reply
	}
	t.result = Result{AgentID: selected.ID, Text: out, Activity: e.Outcome.Activity, Injected: t.injected, Attempt: e.Record.ID}
	completion, pending, err := c.completion(ctx, e.Record, t.result, t.spent.attemptUsage(), t.clock)
	t.pending = pending
	return completion, err
}

func (t *chatTurn) wrap(step lifecycle.Step, e *lifecycle.Execution, err error) error {
	var refused *lifecycle.Refused
	switch {
	case step == lifecycle.StepAdmit && !errors.As(err, &refused):
		// The record's words for an admission that could not be judged,
		// as they were; settle names the host for the user.
		return fmt.Errorf("admission: %w", err)
	case step == lifecycle.StepStart && e.Armed() && e.Record.State != attempt.Running:
		return fmt.Errorf("arm prompt execution: %w", err)
	}
	return err
}

// settle reads how the run ended for the conversation: the session state
// it leaves, the words the user sees, and what lands after a completion.
func (t *chatTurn) settle(parent context.Context, run lifecycle.Result, err error) (Result, error) {
	c, req, selected := t.c, t.req, t.selected
	t.run = run
	t.managed = run.Managed || pendingNodeOpen(run.Record, err) != nil
	var step *lifecycle.StepError
	errors.As(err, &step)
	if run.Record.ID != "" && run.Record.TaskID != "" && c.afterTurn != nil && (step == nil || step.Step >= lifecycle.StepArm) {
		// Last of all — after the attempt is closed and the queued
		// landings are done — whoever waits for this turn's end is told.
		// A turn that ended before its session was armed — refused, or
		// without its before-snapshot — was never a turn to wait for.
		defer c.afterTurn(run.Record.TaskID)
	}
	if step != nil {
		switch step.Step {
		case lifecycle.StepOpen:
			var busy attempt.Busy
			if errors.As(err, &busy) {
				holderAgent, holderTask := busy.Holder, "?"
				if holder, herr := c.attempts.Get(parent, busy.Holder); herr == nil {
					holderAgent, holderTask = holder.Agent, holder.TaskID
				}
				return Result{}, UserError{Text: c.text.T(i18n.ProjectBusy, t.binding.ProjectID, holderAgent, holderTask, protocol.CommandProject)}
			}
			return Result{}, fmt.Errorf("open attempt: %w", step.Err)
		case lifecycle.StepAdmit:
			var refused *lifecycle.Refused
			if errors.As(err, &refused) {
				return Result{}, UserError{Text: c.text.T(i18n.AdmissionRefused, selected.ID, placeLabel(selected.Node), refused.Admission.Unmet())}
			}
			cause := step.Err
			if unwrapped := errors.Unwrap(cause); unwrapped != nil {
				cause = unwrapped
			}
			return Result{}, fmt.Errorf("admission on %s: %w", placeLabel(selected.Node), cause)
		}
	}
	if err == nil {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 2*time.Minute)
		defer cancel()
		if afterErr := c.afterCompletion(ctx, run.Record, t.result, t.pending, t.clock); afterErr != nil {
			slog.Error(fmt.Sprintf("turn: completion: %v", afterErr), "attempt", run.Record.ID, "task", t.tracked, "conversation", req.ConversationID)
			return t.result, afterErr
		}
		return t.result, nil
	}
	if run.Driven && !t.finished {
		switch {
		case errors.Is(err, harness.ErrStopUnconfirmed):
			// Unknown physical writers were classified above and
			// quarantined; a node-owned session keeps its state for the
			// observer that comes back to it.
			if !t.managed {
				if stateErr := c.store.DeleteSession(req.ConversationID, selected.ID); stateErr != nil {
					slog.Error(fmt.Sprintf("turn: delete unconfirmed session: %v", stateErr), "attempt", run.Record.ID, "conversation", req.ConversationID, "agent", selected.ID)
				}
			}
		case errors.Is(err, harness.ErrTurnCanceled):
			// The agent stopped the turn itself (e.g. a permission request
			// was rejected); the session stays consistent, so keep it and
			// clear the taint instead of tearing the process down.
			t.session.Tainted = false
			t.session.InstructionsApplied = true
			if stateErr := c.store.SaveSession(t.session); stateErr != nil {
				slog.Error(fmt.Sprintf("turn: save canceled session state: %v", stateErr), "attempt", run.Record.ID, "conversation", req.ConversationID, "agent", selected.ID)
			}
		default:
			// A failed/expired turn does not authorize killing the shared
			// host; a confirmed response only invalidates this
			// conversation's session.
			if stateErr := c.store.DeleteSession(req.ConversationID, selected.ID); stateErr != nil {
				slog.Error(fmt.Sprintf("turn: delete failed session state: %v", stateErr), "attempt", run.Record.ID, "conversation", req.ConversationID, "agent", selected.ID)
			}
		}
	}
	if t.finished {
		// The completion was assembled and not committed: what the agent
		// said goes back with why, as it always did.
		return t.result, err
	}
	return Result{}, err
}

// unresolved is what the execution scope keeps of a turn whose stop is
// unconfirmed: the node-owned session its observer left, or the open
// whose reply never came.
func (t *chatTurn) unresolved(err error) error {
	if !errors.Is(err, harness.ErrStopUnconfirmed) {
		return nil
	}
	run := t.run
	if run.Managed && run.Session != nil && t.selected.Node != "" {
		return &execution.RetainedObserverDetached{AttemptID: run.Record.ID, NodeID: t.selected.Node, SessionID: run.Session.ID(), Cause: err}
	}
	if pending := pendingNodeOpen(run.Record, err); pending != nil {
		return pending
	}
	return err
}
