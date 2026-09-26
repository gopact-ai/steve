package turn

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/gopact-ai/steve/internal/agentexec"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/lifecycle"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/view"
)

type RetainedChat struct {
	AttemptID    string
	TaskID       string
	Conversation string
	MessageID    string
	AgentID      string
	NodeID       string
	ProjectID    string
	Completed    bool
}

// retainedBlocked leaves the original attempt and exchange unresolved. The
// question describes reconciliation work; it is not an invented native-agent
// callback and accepting it only retries observation of the retained command.
// reason is already in the coordinator's language: a catalog entry or the
// text of the error that stopped the check.
func (c *Coordinator) retainedBlocked(code string, attempted, problem i18n.Key, reason string, recommendation i18n.Key, cause error) *agentexec.RecoveryBlocked {
	message := c.text.T(i18n.RecoveryTried, c.text.T(attempted)) + "\n\n" + c.text.T(problem) + "\n\n" + reason + "\n\n" + c.text.T(recommendation)
	return &agentexec.RecoveryBlocked{Cause: cause, Question: agentexec.RecoveryQuestion("recovery/"+code, c.text.T(i18n.RecoveryTitleTask), message,
		view.Choice{Label: c.text.T(i18n.RecoveryRetry), Detail: c.text.T(i18n.RecoveryRetryTask)},
		view.Choice{Label: c.text.T(i18n.RecoveryWait), Detail: c.text.T(i18n.RecoveryWaitReconnect)})}
}

// retainedChat says whether a chat execution holds an accepted native command
// or a committed but undelivered result that recovery observes again.
func retainedChat(r attempt.Record) bool {
	if r.Kind != attempt.KindChat || r.State == attempt.Superseded {
		return false
	}
	reattachable := attempt.Relocatable(r) || attempt.PreparingRelocation(r) || pendingChatOpen(r)
	if r.State != attempt.Bound && !nodewire.IsManagedSession(r.Session) && !reattachable {
		return false
	}
	return r.State == attempt.Running || r.State.Terminal() || reattachable
}

// RetainedChatsFor identifies the retained executions of one exchange: its
// conversation and the message that opened the turn. More than one is
// returned as found, never chosen between. It creates neither tasks nor
// replacement attempts, and reads only the turn's own attempts.
func (c *Coordinator) RetainedChatsFor(ctx context.Context, conversation, messageID string) ([]RetainedChat, error) {
	records, err := c.attempts.ForTurn(ctx, messageID)
	if err != nil {
		return nil, err
	}
	var result []RetainedChat
	for _, r := range records {
		if !retainedChat(r) {
			continue
		}
		tracked, ok := c.tasks.Get(r.TaskID)
		if !ok || tracked.Channel != conversation {
			continue
		}
		result = append(result, RetainedChat{AttemptID: r.ID, TaskID: r.TaskID, Conversation: tracked.Channel, MessageID: r.TurnID, AgentID: r.Agent, NodeID: r.Node, ProjectID: r.Project, Completed: r.State.Terminal()})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].AttemptID < result[j].AttemptID })
	return result, nil
}

// CheckRetainedChats fails when execution history cannot be read, so retained
// recovery does not start over records it could not classify. It decodes no
// settled payloads.
func (c *Coordinator) CheckRetainedChats(ctx context.Context) error {
	return c.attempts.CheckReadable(ctx)
}

type retainedRuntime interface {
	AttachRetainedSession(context.Context, harness.Placement, string, string) (harness.ResumableRunner, error)
}

// ProbeRetained reports whether the retained execution can be reached
// again. It reads the node's own view of the session and nothing else:
// no attempt is adopted, settled, relocated or prompted, so a recovery
// that is waiting on the owner can use it to notice the original coming
// back and carry on without an answer.
func (c *Coordinator) ProbeRetained(ctx context.Context, id string) error {
	c.requestMu.RLock()
	defer c.requestMu.RUnlock()
	record, err := c.attempts.Get(ctx, id)
	if err != nil {
		return err
	}
	if record.State != attempt.Running || !nodewire.IsManagedSession(record.Session) || record.Node == "" {
		return errors.New("retained execution has no node-owned session to rejoin")
	}
	evidence, err := c.inspectRelocation(ctx, record)
	if err != nil {
		return err
	}
	if evidence.Session.ID != record.Session || evidence.Session.Command == nil {
		return errors.New("node does not hold the original command")
	}
	if evidence.Session.ProcessStopped || evidence.Session.Command.State == nodewire.SessionCommandUncertain {
		return errors.New("node cannot vouch for the original command")
	}
	return nil
}

// ResumeRetainedChat observes the exact command admitted before coordinator
// loss. It never calls Handle, opens a new task/attempt, or sends Prompt again.
func (c *Coordinator) ResumeRetainedChat(parent context.Context, id string, req Request) (result Result, err error) {
	c.requestMu.RLock()
	defer c.requestMu.RUnlock()
	return c.resumeRetainedChat(parent, id, req, false)
}

func (c *Coordinator) resumeRetainedChat(parent context.Context, id string, req Request, turnOwned bool) (result Result, err error) {
	c, err = c.forChannel(req.Channel)
	if err != nil {
		return Result{}, err
	}
	if req.Locale != "" {
		c = c.localized(i18n.FromLang(req.Locale))
	}
	if c.maintaining {
		return Result{}, c.retainedBlocked("maintenance", i18n.RetainedTriedCurrentCoordinator, i18n.RetainedProblemCoordinatorUnavailable, c.text.T(i18n.RetainedReasonHandover), i18n.RetainedAdviceAwaitHandoverRecheck, nil)
	}
	record, err := c.retainedChatRecord(parent, id, req)
	if err != nil {
		return Result{}, err
	}
	if delivered, err, ok := c.deliverRetainedChat(parent, req, record); ok {
		return delivered, err
	}
	if record.State != attempt.Running || record.Execution == nil {
		return Result{}, c.retainedBlocked("state", i18n.RetainedTriedExecutionState, i18n.RetainedProblemNotResumable, c.text.T(i18n.RetainedReasonNoRunState), i18n.RetainedAdviceCheckExecution, nil)
	}
	if err := c.tasks.CheckExecution(*record.Execution); err != nil {
		return Result{}, c.retainedBlocked("authorization", i18n.RetainedTriedTaskAuthority, i18n.RetainedProblemAuthorityChanged, c.text.T(i18n.RetainedReasonTaskStopped), i18n.RetainedAdviceCheckTaskState, err)
	}
	manager, ok := c.runtime.(retainedRuntime)
	if !ok {
		return Result{}, c.retainedBlocked("runtime", i18n.RetainedTriedResumeInterface, i18n.RetainedProblemRuntime, c.text.T(i18n.RetainedReasonNoResumeInterface), i18n.RetainedAdviceCheckNodeService, nil)
	}
	// The observer runs under the prompt timeout like the prompt it
	// resumes: a command that stays silent ends the turn as a timeout.
	// A clock that runs out before the observer has attached stops no
	// command: the execution stays retained for the next observation.
	clock, expire, touch := c.newIdleClock(parent, record.Node)
	defer expire()
	ctx, cancel := context.WithCancel(clock)
	defer cancel()
	if !turnOwned {
		if !c.beginTurn(req.ConversationID, record.Agent, cancel) {
			return Result{}, c.retainedBlocked("busy", i18n.RetainedTriedSessionOccupancy, i18n.RetainedProblemSessionBusy, c.text.T(i18n.RetainedReasonOneDriver), i18n.RetainedAdviceAwaitDriver, nil)
		}
		defer c.clearActive(req.ConversationID, record.Agent)
	}
	defer func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if entry := c.cancels[sessionKey(req.ConversationID, record.Agent)]; entry != nil {
			entry.err = err
		}
	}()
	scope, err := c.executions.BeginAccepted(ctx, execution.Key{TaskID: record.TaskID, InstanceID: record.TurnID, AttemptID: record.ID}, record.Execution)
	if err != nil {
		return Result{}, err
	}
	// This consumer only observes the committed ns_ execution. Even when
	// attach fails, a service shutdown can join its local observer without
	// claiming the remote process stopped. Explicit task Stop still reports
	// its unresolved state and the durable attempt remains quarantined.
	knownRetained := record.Node != "" && nodewire.IsManagedSession(record.Session)
	settled := false
	var cleanupFailure error
	defer func() {
		scope.Finish(retainedUnresolved(record, knownRetained, settled, cleanupFailure, err, ctx.Err(), false))
	}()
	ctx = scope.Context()
	if req.OnTurnReady != nil {
		req.OnTurnReady(record.TaskID, record.ID)
	}
	runner, observed, refreshed, err := c.attachRetainedChat(ctx, manager, record)
	if err != nil {
		return Result{}, err
	}
	record = refreshed
	knownRetained = true
	if err := c.bindExecutionGate(ctx, req.ConversationID, record.ID); err != nil {
		return Result{}, err
	}
	settled = observed.Command != nil && observed.Command.Settled && (observed.Command.State == nodewire.SessionCommandCompleted || observed.Command.State == nodewire.SessionCommandCancelled)
	scope.AdoptRetained()
	c.setRunner(req.ConversationID, record.Agent, runner)
	spent := &turnSpend{resetIdle: touch}
	req.OnProgress = spent.wrap(req.OnProgress, record.Agent)
	req.phase(view.PhaseRunning)
	t := &retainedTurn{c: c, req: req, record: record, spent: spent, finishing: true, injected: &Injected{Project: record.Project, Workspace: record.Workspace.Path, Agent: record.Agent, Node: record.Node, Harness: record.Harness, Session: record.Session}}
	run, runErr := lifecycle.Reattach(ctx, t.options("", true), record, runner)
	settled = run.Settled
	result, cleanupFailure, runErr = t.settle(ctx, run, runErr)
	if cleanupFailure != nil {
		return Result{}, cleanupFailure
	}
	var detached *execution.RetainedObserverDetached
	if errors.As(runErr, &detached) {
		return Result{}, c.retainedBlocked("observer-detached", i18n.RetainedTriedObserve, i18n.RetainedProblemObserverLost, c.text.T(i18n.RetainedReasonKeepUntilVerified), i18n.RetainedAdviceReconnect, runErr)
	}
	if saved := c.store.Conversation(req.ConversationID).Sessions[record.Agent]; saved.UpstreamID == record.Session {
		saved.Tainted = false
		saved.InstructionsApplied = true
		if err := c.store.SaveSession(saved); err != nil {
			cleanupFailure = err
			return Result{}, err
		}
	}
	if cleanupFailure = c.finishRetainedTask(record, runErr, spent); cleanupFailure != nil {
		return Result{}, cleanupFailure
	}
	c.notifyAccountedTurn(record.TaskID)
	if runErr != nil {
		return result, runErr
	}
	return c.gateDisclosure(ctx, req, result)
}

func (c *Coordinator) finishRetainedTask(record attempt.Record, runErr error, spent *turnSpend) error {
	if spent != nil {
		return c.tasks.SettleAttempt(context.Background(), record.TaskID, record.ID, record.TurnID, time.Now().UTC(), lifecycle.OutcomeOf(runErr), task.RecoveryUsage{Tokens: spent.tokens(), Model: spent.model(), Reported: spent.attemptUsage().Reported})
	}
	var tokens task.Tokens
	model := ""
	if record.Usage != nil {
		u := record.Usage
		tokens = task.Tokens{Input: u.Input, Output: u.Output, CachedRead: u.CachedRead, CachedWrite: u.CachedWrite, Total: u.Input + u.Output}
		model = u.Model
	}
	return c.tasks.SettleAttempt(context.Background(), record.TaskID, record.ID, record.TurnID, record.EndedAt, lifecycle.OutcomeOf(runErr), task.RecoveryUsage{Tokens: tokens, Model: model, Reported: record.Usage != nil && record.Usage.Reported})
}

// retainedChatRecord is the attempt a retained chat resumes, once it is
// this conversation's, this requester's, and has a node-owned session to
// go back to.
func (c *Coordinator) retainedChatRecord(parent context.Context, id string, req Request) (attempt.Record, error) {
	record, err := c.attempts.Get(parent, id)
	if err != nil {
		return attempt.Record{}, err
	}
	tracked, ok := c.tasks.Get(record.TaskID)
	if !ok || record.Kind != attempt.KindChat || tracked.Channel != req.ConversationID || record.TurnID != req.MessageID {
		return attempt.Record{}, c.retainedBlocked("identity", i18n.RetainedTriedTaskSession, i18n.RetainedProblemIdentity, c.text.T(i18n.RetainedReasonIDMismatch), i18n.RetainedAdviceCheckTaskRecord, nil)
	}
	if tracked.Requester != "" && tracked.Requester != req.SenderOpenID {
		return attempt.Record{}, errors.New("retained execution belongs to another requester")
	}
	c.rememberMode(req)
	if pendingChatOpen(record) {
		return attempt.Record{}, c.inspectPendingOpen(parent, record)
	}
	if record.State != attempt.Bound && !nodewire.IsManagedSession(record.Session) && !endedBeforeSession(record) {
		return attempt.Record{}, c.retainedBlocked("native-identity", i18n.RetainedTriedSessionID, i18n.RetainedProblemNoNativeSession, c.text.T(i18n.RetainedReasonPreparationBroke), i18n.RetainedAdviceRecheckRecovery, nil)
	}
	return record, nil
}

// endedBeforeSession is an attempt that is over, whose stop is settled, and
// that never recorded a native session: nothing of it is retained on any
// node, so a missing native identity is not something to wait for. What it
// recorded is delivered instead. An attempt still live, an unconfirmed stop,
// or any recorded session keeps the identity requirement.
func endedBeforeSession(record attempt.Record) bool {
	return record.State.Terminal() && record.State != attempt.Bound && !record.Unsettled && record.Session == ""
}

// deliverRetainedChat delivers what an already ended attempt committed: a
// result, or a failure. An ended attempt is a delivery fact; it cannot
// authorize replaying the original native prompt.
func (c *Coordinator) deliverRetainedChat(parent context.Context, req Request, record attempt.Record) (Result, error, bool) {
	if record.State == attempt.Bound {
		var result Result
		if record.Result == nil || len(record.Result.Output) == 0 || json.Unmarshal(record.Result.Output, &result) != nil {
			return Result{}, c.retainedBlocked("result", i18n.RetainedTriedReadResult, i18n.RetainedProblemReplyMissing, c.text.T(i18n.RetainedReasonNoRerunForReply), i18n.RetainedAdviceCheckArtifacts, nil), true
		}
		result.Attempt = record.ID
		if err := c.SettleChatAccounting(parent, record.ID); err != nil {
			return Result{}, err, true
		}
		c.notifyAccountedTurn(record.TaskID)
		result, err := c.gateDisclosure(parent, req, result)
		return result, err, true
	}
	if record.State.Terminal() && record.Unsettled {
		return Result{}, c.retainedBlocked("terminal-unsettled", i18n.RetainedTriedEndEvidence, i18n.RetainedProblemStopUnconfirmed, c.text.T(i18n.RetainedReasonNoStopProof), i18n.RetainedAdviceCheckProcess, harness.ErrStopUnconfirmed), true
	}
	if !record.State.Terminal() {
		return Result{}, nil, false
	}
	if record.Error == "" {
		return Result{}, c.retainedBlocked("terminal", i18n.RetainedTriedReadEnded, i18n.RetainedProblemNoResult, c.text.T(i18n.RetainedReasonResendRepeats), i18n.RetainedAdviceCheckBeforeNext, nil), true
	}
	result := Result{AgentID: record.Agent, Text: record.Error, Attempt: record.ID}
	failure := errors.New(record.Error)
	if err := c.finishRetainedTask(record, failure, nil); err != nil {
		return Result{}, err, true
	}
	return result, failure, true
}

// attachRetainedChat finds the node-owned session again by its receipt and
// proves the attempt is still the one it was admitted as.
func (c *Coordinator) attachRetainedChat(ctx context.Context, manager retainedRuntime, record attempt.Record) (harness.ResumableRunner, nodewire.SessionState, attempt.Record, error) {
	runner, err := manager.AttachRetainedSession(ctx, harness.Placement{Node: record.Node, Harness: record.Harness}, record.Session, record.Workspace.Path)
	if err != nil {
		return nil, nodewire.SessionState{}, record, c.retainedBlocked("node-attach", i18n.RetainedTriedAttachNode, i18n.RetainedProblemAttach, c.text.T(i18n.RetainedReasonNodeUnreachable), i18n.RetainedAdviceReconnectNode, err)
	}
	inspector, ok := runner.(harness.RetainedSessionInspector)
	if !ok {
		return nil, nodewire.SessionState{}, record, errors.New("retained runner cannot inspect its command binding")
	}
	observed, err := inspector.InspectRetained(ctx)
	if err != nil {
		return nil, nodewire.SessionState{}, record, c.retainedBlocked("node-state", i18n.RetainedTriedReadNodeState, i18n.RetainedProblemStateUnknown, c.text.T(i18n.RetainedReasonConnectNoProof), i18n.RetainedAdviceAwaitNode, err)
	}
	if observed.Command != nil && observed.Command.State == nodewire.SessionCommandUncertain {
		return nil, nodewire.SessionState{}, record, c.retainedBlocked("native-interrupted", i18n.RetainedTriedNodeInterrupted, i18n.RetainedProblemNoLiveRun, c.text.T(i18n.RetainedReasonNodeRestarted), i18n.RetainedAdviceCheckDone, harness.ErrStopUnconfirmed)
	}
	refreshed, err := c.attempts.RecoverRetained(ctx, record.ID, attempt.RetainedEvidence{ObservedAt: time.Now(), Session: observed})
	if err != nil {
		reason := c.text.T(i18n.RetainedReasonEvidenceMismatch)
		switch {
		case errors.Is(err, task.ErrExecutionStopped):
			reason = c.text.T(i18n.RetainedReasonEvidenceStopped)
		case errors.Is(err, ledger.ErrStale):
			reason = c.text.T(i18n.RetainedReasonEvidenceStale)
		case errors.Is(err, ledger.ErrConflict):
			reason = c.text.T(i18n.RetainedReasonEvidenceConflict)
		}
		return nil, nodewire.SessionState{}, record, c.retainedBlocked("retained-evidence", i18n.RetainedTriedEvidence, i18n.RetainedProblemUnsafe, reason, i18n.RetainedAdviceCheckNodeRecords, err)
	}
	return runner, observed, refreshed, nil
}

// retainedUnresolved is what a retained turn's execution scope keeps when
// the turn ends without settling: the node-owned session its observer
// left, the open whose reply never came, or the cleanup that failed. A
// service shutdown can join a detached observer without claiming the
// remote process stopped; an explicit task Stop still reports it.
func retainedUnresolved(record attempt.Record, known, settled bool, cleanupFailure, err, ctxErr error, opening bool) error {
	detached := func(cause error) error {
		return &execution.RetainedObserverDetached{AttemptID: record.ID, NodeID: record.Node, SessionID: record.Session, Cause: cause}
	}
	if cleanupFailure != nil {
		if known && ctxErr != nil {
			return detached(errors.Join(cleanupFailure, ctxErr))
		}
		return cleanupFailure
	}
	if settled || err == nil {
		return nil
	}
	unresolved := errors.Join(harness.ErrStopUnconfirmed, err)
	if known {
		return detached(unresolved)
	}
	if opening {
		if pending := pendingNodeOpen(record, err); pending != nil {
			return pending
		}
	}
	return unresolved
}
