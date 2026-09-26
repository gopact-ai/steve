package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/lifecycle"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/text"
	"github.com/gopact-ai/steve/internal/view"
)

// QuestionBinding names the existing child execution — node-owned or on the
// hub — that asked. Recovery diagnostics use a separate callback and never
// approve it.
type QuestionBinding struct {
	Conversation, ParentTask, Task, Attempt, Node, Agent, Project, Session string
	// Transport is the parent's channel, which names a recovery
	// conversation when the parent's own is not a console one.
	Transport string
}

type RecoveryQuestion struct {
	QuestionBinding
	Question view.Question
}

// defaultRecoveryQuiet is the stretch a child's recovery works through on
// its own: long enough to cover a node restart or a dropped link, short
// enough that a node which is really gone is reported while the owner
// still remembers asking for the work.
const defaultRecoveryQuiet = 90 * time.Second

// recoveryNotice is what a child's recovery last reported: why it could
// not be joined, since when it has been failing without interruption,
// and whether that has already been put to the parent's owner.
type recoveryNotice struct {
	code   string
	since  time.Time
	asked  bool
	cancel context.CancelFunc
}

type retainedRuntime interface {
	AttachRetainedSession(context.Context, harness.Placement, string, string) (harness.ResumableRunner, error)
}

// SetRecoveryQuestion delivers platform diagnostics into the parent's original
// conversation. A response asks to reconcile again; it never becomes a tool grant.
func (s *Service) SetRecoveryQuestion(handler func(context.Context, RecoveryQuestion) (view.Answer, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, notice := range s.recoveryQuestions {
		if notice.cancel != nil {
			notice.cancel()
		}
	}
	s.recoveryQuestions = map[string]*recoveryNotice{}
	s.recoveryQuestion = handler
}

// SetQuestionHandlers installs who answers a child's own questions and
// permission requests: the owner, reached with the child's execution
// binding whether the child runs on a node or on the hub itself.
func (s *Service) SetQuestionHandlers(
	ask func(context.Context, QuestionBinding, permission.Ask) (acp.RequestPermissionOutcome, error),
	askUser func(context.Context, QuestionBinding, view.Question) (view.Answer, error),
) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ownerPermission, s.ownerQuestion = ask, askUser
}

func questionBinding(parent, child task.Task, record attempt.Record) QuestionBinding {
	return QuestionBinding{Conversation: parent.Channel, Transport: parent.Transport, ParentTask: parent.ID, Task: child.ID, Attempt: record.ID, Node: record.Node, Agent: record.Agent, Project: record.Project, Session: record.Session}
}

func (s *Service) ownerHandlers(binding QuestionBinding) (permission.AskFunc, acphost.AskUserFunc) {
	s.mu.Lock()
	ask, askUser := s.ownerPermission, s.ownerQuestion
	s.mu.Unlock()
	var permissionHandler permission.AskFunc
	var questionHandler acphost.AskUserFunc
	if ask != nil {
		permissionHandler = func(ctx context.Context, q permission.Ask) (acp.RequestPermissionOutcome, error) {
			return ask(ctx, binding, q)
		}
	}
	if askUser != nil {
		questionHandler = func(ctx context.Context, q view.Question) (view.Answer, error) { return askUser(ctx, binding, q) }
	}
	return permissionHandler, questionHandler
}

func retainedDetached(record attempt.Record, cause error) error {
	return &execution.RetainedObserverDetached{AttemptID: record.ID, NodeID: record.Node, SessionID: record.Session, Cause: errors.Join(harness.ErrStopUnconfirmed, cause)}
}

// RecoverRetained starts one observer per admitted child. The caller owns its
// retry schedule; this method neither starts a timer nor waits for child output.
func (s *Service) RecoverRetained(ctx context.Context) error {
	if s.tasks == nil || s.attempts == nil || s.artifacts == nil || s.executions == nil {
		return errors.New("retained delegate recovery is not configured")
	}
	if err := s.resolveJoined(ctx); err != nil {
		return err
	}
	records, err := s.recoverableRecords(ctx)
	if err != nil {
		return err
	}
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return err
		}
		if record.Kind != attempt.KindDelegate || (!nodewire.IsManagedSession(record.Session) && !pendingDelegatePreparation(record)) {
			continue
		}
		tracked, ok := s.tasks.Get(record.TaskID)
		if !ok || !tracked.Delegated() || tracked.Parent == "" || tracked.DelegationSettled() {
			continue
		}
		parent, ok := s.tasks.Get(tracked.Parent)
		if !ok {
			continue
		}
		if tracked.State == task.StatePaused || tracked.State == task.StateCancelled || parent.State == task.StatePaused || parent.State == task.StateCancelled {
			s.clearRecovery(record.ID)
			continue
		}
		if record.Execution != nil && s.tasks.CheckExecution(*record.Execution) != nil {
			s.clearRecovery(record.ID)
			continue
		}
		if pendingDelegatePreparation(record) {
			s.reportDelegatePreparation(ctx, parent, tracked, record)
			continue
		}
		s.mu.Lock()
		if current := s.pending[tracked.ID]; current != nil {
			select {
			case <-current.done:
				delete(s.pending, tracked.ID)
			default:
				s.mu.Unlock()
				continue
			}
		}
		entry := &child{started: record.StartedAt, done: make(chan struct{}), session: record.Session, result: agentmcp.DelegateResult{TaskID: tracked.ID, Agent: record.Agent, Node: record.Node, State: task.StateRunning}}
		s.pending[tracked.ID] = entry
		s.mu.Unlock()
		s.rememberAttempt(tracked.ID, record.ID)
		go s.recoverChild(s.executions.Detached(ctx), parent, tracked, record, entry)
	}
	return s.settleUnrecordedChildren(ctx)
}

// recoverableRecords lists, once each, the attempts a recovery pass may
// resume: every live attempt, most recently updated first, then the settled
// attempts of delegated children whose result is not yet settled, most
// recently updated first. A malformed row fails the whole pass.
func (s *Service) recoverableRecords(ctx context.Context) ([]attempt.Record, error) {
	live, err := s.attempts.Live(ctx)
	if err != nil {
		return nil, err
	}
	settled, err := s.attempts.ForTasksByUpdate(ctx, s.tasks.PendingDelegations())
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(live))
	out := make([]attempt.Record, 0, len(live)+len(settled))
	for _, record := range live {
		if !seen[record.ID] {
			seen[record.ID] = true
			out = append(out, record)
		}
	}
	for _, record := range settled {
		if record.State.Terminal() && !seen[record.ID] {
			seen[record.ID] = true
			out = append(out, record)
		}
	}
	return out, nil
}

// recoveryPending reports one way a child could not be joined: a question
// to the parent's conversation, and the child left detached.
type recoveryPending func(code string, attempted, problem, reason, recommendation i18n.Key, cause error)

// recoverChild joins a delegated child's node-owned execution again. What
// the attempt already committed is delivered from the record; a running
// one is attached, reconciled against the node's evidence, and reattached
// from the prompt on. Every way it cannot be joined is a recovery question
// to the parent, never a second prompt.
func (s *Service) recoverChild(ctx context.Context, parent, tracked task.Task, record attempt.Record, entry *child) {
	binding := questionBinding(parent, tracked, record)
	pending := func(code string, attempted, problem, reason, recommendation i18n.Key, cause error) {
		s.reportRecovery(ctx, binding, code, attempted, problem, reason, recommendation)
		s.completeChild(ctx, parent.Channel, parent, tracked, tracked.Goal, entry, entry.result, retainedDetached(record, cause), view.Progress{})
	}
	if record.Execution == nil || record.Execution.TaskID != tracked.ID || record.Project != tracked.ProjectID || record.Agent != tracked.Member || record.Node != tracked.Node {
		pending("identity", i18n.DelegateRecoveryTriedIdentity, i18n.DelegateRecoveryProblemIdentity, i18n.DelegateRecoveryReasonIdentity, i18n.DelegateRecoveryAdviceIdentity, nil)
		return
	}
	scope, err := s.executions.BeginAccepted(ctx, execution.Key{TaskID: tracked.ID, InstanceID: record.TurnID, AttemptID: record.ID}, record.Execution)
	if err != nil {
		pending("authorization", i18n.DelegateRecoveryTriedAuthorization, i18n.DelegateRecoveryProblemAuthorization, i18n.DelegateRecoveryReasonAuthorization, i18n.DelegateRecoveryAdviceAuthorization, err)
		return
	}
	entry.scope = scope
	ctx = scope.Context()
	if s.deliverRecovered(ctx, parent, tracked, record, entry, pending) {
		return
	}
	runner, ask, askUser, ok := s.attachChild(ctx, parent, tracked, &record, entry, scope, binding, pending)
	if !ok {
		return
	}
	s.clearRecovery(record.ID)
	d := &delegation{service: s, parent: parent, child: tracked, at: harness.Placement{Node: record.Node, Harness: record.Harness}, binding: binding}
	run, runErr := lifecycle.Reattach(ctx, lifecycle.Options{
		Attempts: s.attempts, Roster: s.roster, Sessions: s.sessions, Workspaces: s.artifacts, Actor: "delegate-recovery",
		Spec: record.Spec, At: d.at, Resume: true, Ask: ask, AskUser: askUser,
		Observe: func(progress view.Progress) {
			s.report(Child{Transport: parent.Transport, Conversation: parent.Channel, ParentTask: parent.ID, Task: tracked.ID, Agent: record.Agent, Node: record.Node, Goal: tracked.Goal, State: task.StateRunning, Since: record.StartedAt, Elapsed: time.Since(record.StartedAt), Attempt: record.ID}, progress)
		},
		Finish: d.finish, Failed: d.failed,
		// The node keeps the session. An observer that cannot vouch for
		// the end, or was cancelled, leaves the record as it is and asks:
		// the question is what quarantines it. The completion publish
		// prepared is committed as given, as completeResult did.
		Settlement: lifecycle.Settlement{Quarantine: lifecycle.QuarantineManaged, DetachManaged: true, Detachment: lifecycle.DetachSilently, CancelDetaches: true, CommitAsGiven: true},
	}, record, runner)
	s.settleRecovered(ctx, parent, tracked, record, entry, d, run, runErr, pending)
}

// deliverRecovered delivers what an attempt that already ended committed —
// its result, or its failure — and asks about one that is neither over nor
// running. An ended attempt is a delivery fact; it cannot authorize
// replaying the original prompt.
func (s *Service) deliverRecovered(ctx context.Context, parent, tracked task.Task, record attempt.Record, entry *child, pending recoveryPending) bool {
	if record.State == attempt.Bound {
		var result agentmcp.DelegateResult
		if record.Result == nil || len(record.Result.Output) == 0 || json.Unmarshal(record.Result.Output, &result) != nil || result.TaskID != tracked.ID || result.Agent != record.Agent || result.Node != record.Node {
			pending("result", i18n.DelegateRecoveryTriedResult, i18n.DelegateRecoveryProblemResult, i18n.DelegateRecoveryReasonResult, i18n.DelegateRecoveryAdviceResult, nil)
			return true
		}
		s.clearRecovery(record.ID)
		if record.Result.Artifact != "" && slices.Contains(result.Refs, "artifact "+record.Result.Artifact) {
			if err := s.artifacts.Defer(ctx, record.Project, record.Result.Artifact, "task #"+tracked.ID, artifact.SourceOf(ctx, record.ID)...); err != nil {
				pending("landing", i18n.DelegateRecoveryTriedLanding, i18n.DelegateRecoveryProblemLanding, i18n.DelegateRecoveryReasonLanding, i18n.DelegateRecoveryAdviceLanding, err)
				return true
			}
		}
		s.completeChild(ctx, parent.Channel, parent, tracked, tracked.Goal, entry, result, nil, view.Progress{})
		return true
	}
	if record.State.Terminal() && !record.Unsettled && record.Error != "" {
		result := agentmcp.DelegateResult{TaskID: tracked.ID, Agent: record.Agent, Node: record.Node, Answer: record.Error}
		if record.Result != nil && len(record.Result.Output) > 0 {
			if json.Unmarshal(record.Result.Output, &result) != nil || result.TaskID != tracked.ID || result.Agent != record.Agent || result.Node != record.Node {
				pending("failed-output", i18n.DelegateRecoveryTriedFailedOutput, i18n.DelegateRecoveryProblemFailedOutput, i18n.DelegateRecoveryReasonFailedOutput, i18n.DelegateRecoveryAdviceFailedOutput, nil)
				return true
			}
		}
		// The failure is settled from the record, like a bound result:
		// a question raised while the node was unreachable is answered by
		// it and must not stay open over a child that is over.
		s.clearRecovery(record.ID)
		s.completeChild(ctx, parent.Channel, parent, tracked, tracked.Goal, entry, result, recordedFailure(result, record.Error), view.Progress{})
		return true
	}
	if record.State != attempt.Running {
		pending("state", i18n.DelegateRecoveryTriedState, i18n.DelegateRecoveryProblemState, i18n.DelegateRecoveryReasonState, i18n.DelegateRecoveryAdviceState, nil)
		return true
	}
	return false
}

// attachChild joins the child's node-owned session: the node's evidence is
// checked against the record in one ledger transaction, the child's tool
// authorization is bound again, and its pending questions must have
// someone to answer them before the observer takes over.
func (s *Service) attachChild(ctx context.Context, parent, tracked task.Task, record *attempt.Record, entry *child, scope *execution.Scope, binding QuestionBinding, pending recoveryPending) (harness.ResumableRunner, permission.AskFunc, acphost.AskUserFunc, bool) {
	manager, ok := s.sessions.(retainedRuntime)
	if !ok {
		pending("runtime", i18n.DelegateRecoveryTriedRuntime, i18n.DelegateRecoveryProblemRuntime, i18n.DelegateRecoveryReasonRuntime, i18n.DelegateRecoveryAdviceRuntime, nil)
		return nil, nil, nil, false
	}
	runner, err := manager.AttachRetainedSession(ctx, harness.Placement{Node: record.Node, Harness: record.Harness}, record.Session, record.Workspace.Path)
	if err != nil {
		pending("attach", i18n.DelegateRecoveryTriedAttach, i18n.DelegateRecoveryProblemAttach, i18n.DelegateRecoveryReasonAttach, i18n.DelegateRecoveryAdviceAttach, err)
		return nil, nil, nil, false
	}
	inspector, ok := runner.(harness.RetainedSessionInspector)
	if !ok {
		pending("evidence", i18n.DelegateRecoveryTriedEvidence, i18n.DelegateRecoveryProblemEvidence, i18n.DelegateRecoveryReasonEvidence, i18n.DelegateRecoveryAdviceEvidence, nil)
		return nil, nil, nil, false
	}
	state, err := inspector.InspectRetained(ctx)
	if err != nil {
		pending("inspect", i18n.DelegateRecoveryTriedInspect, i18n.DelegateRecoveryProblemInspect, i18n.DelegateRecoveryReasonInspect, i18n.DelegateRecoveryAdviceInspect, err)
		return nil, nil, nil, false
	}
	recovered, err := s.attempts.RecoverRetained(ctx, record.ID, attempt.RetainedEvidence{ObservedAt: time.Now(), Session: state})
	if err != nil {
		if s.endStoppedChild(ctx, parent, tracked, *record, entry, state, pending) {
			return nil, nil, nil, false
		}
		pending("reconcile", i18n.DelegateRecoveryTriedReconcile, i18n.DelegateRecoveryProblemReconcile, i18n.DelegateRecoveryReasonReconcile, i18n.DelegateRecoveryAdviceReconcile, err)
		return nil, nil, nil, false
	}
	*record = recovered
	if err := s.bindDelegatedExecution(ctx, parent, tracked, *record); err != nil {
		pending("messaging", i18n.DelegateRecoveryTriedMessaging, i18n.DelegateRecoveryProblemMessaging, i18n.DelegateRecoveryReasonMessaging, i18n.DelegateRecoveryAdviceMessaging, err)
		return nil, nil, nil, false
	}
	scope.AdoptRetained()
	ask, askUser := s.ownerHandlers(binding)
	for _, q := range state.Questions {
		if q.State == "pending" && ((q.Permission != nil && ask == nil) || (q.Permission == nil && askUser == nil)) {
			pending("question", i18n.DelegateRecoveryTriedQuestion, i18n.DelegateRecoveryProblemQuestion, i18n.DelegateRecoveryReasonQuestion, i18n.DelegateRecoveryAdviceQuestion, nil)
			return nil, nil, nil, false
		}
	}
	return runner, ask, askUser, true
}

// endStoppedChild closes a child whose node reports that the process behind
// it is gone. Its command never said how it finished, so there is nothing
// left to wait for and nothing to deliver as a result: a stop receipt settles
// that physical fact, and the child ends like any other settled failure, which
// is what lets its parent run the work again. A node that only lost its
// connection says nothing about the process and is left alone.
func (s *Service) endStoppedChild(ctx context.Context, parent, tracked task.Task, record attempt.Record, entry *child, state nodewire.SessionState, pending recoveryPending) bool {
	command := state.Command
	if !state.ProcessStopped || command == nil || command.ID != attempt.InputCommandID(record) || !command.ProcessStopped || command.Settled {
		return false
	}
	// The ending is recorded, and later delivered, in the service's language.
	stopped, err := s.attempts.ConfirmProcessStopped(i18n.WithLocale(ctx, s.text.Locale()), record.ID, "delegate-recovery", attempt.RetainedEvidence{ObservedAt: time.Now(), Session: state})
	if err != nil {
		slog.Error(fmt.Sprintf("delegate: stopped child not settled task=%s attempt=%s error=%v", tracked.ID, record.ID, err), "parent", parent.ID, "node", record.Node)
		return false
	}
	slog.Warn(fmt.Sprintf("delegate: child ended by a stopped node process task=%s attempt=%s", tracked.ID, record.ID), "parent", parent.ID, "conversation", parent.Channel, "node", record.Node)
	return s.deliverRecovered(ctx, parent, tracked, stopped, entry, pending)
}

// settleRecovered reads how the reattached run ended for the child and its
// parent: a detached observer asks, a recorded end is delivered.
func (s *Service) settleRecovered(ctx context.Context, parent, tracked task.Task, record attempt.Record, entry *child, d *delegation, run lifecycle.Result, runErr error, pending recoveryPending) {
	var step *lifecycle.StepError
	errors.As(runErr, &step)
	var detached *execution.RetainedObserverDetached
	if errors.As(runErr, &detached) {
		// Finish ran when a publication, or its failure, is on the delegation.
		finished := d.publishErr != nil || d.published.result.TaskID != ""
		switch {
		case step != nil && step.Step == lifecycle.StepSettle:
			pending("settlement", i18n.DelegateRecoveryTriedSettlement, i18n.DelegateRecoveryProblemSettlement, i18n.DelegateRecoveryReasonSettlement, i18n.DelegateRecoveryAdviceSettlement, runErr)
		case step != nil && step.Step == lifecycle.StepFinish && !finished:
			s.spent(tracked.ID, run.Last)
			pending("failed-result", i18n.DelegateRecoveryTriedFailedResult, i18n.DelegateRecoveryProblemFailedResult, i18n.DelegateRecoveryReasonFailedResult, i18n.DelegateRecoveryAdviceFailedResult, runErr)
		case step != nil && step.Step == lifecycle.StepFinish:
			s.spent(tracked.ID, run.Last)
			pending("completion", i18n.DelegateRecoveryTriedCompletion, i18n.DelegateRecoveryProblemCompletion, i18n.DelegateRecoveryReasonCompletion, i18n.DelegateRecoveryAdviceCompletion, runErr)
		default:
			pending("observer", i18n.DelegateRecoveryTriedObserver, i18n.DelegateRecoveryProblemObserver, i18n.DelegateRecoveryReasonObserver, i18n.DelegateRecoveryAdviceObserver, errors.Join(runErr, ctx.Err()))
		}
		return
	}
	s.spent(tracked.ID, run.Last)
	if s.canSettleStopped(ctx, runErr) {
		// An explicit stop of a settled command is delivered on a context
		// the stop did not cancel.
		var cancel context.CancelFunc
		ctx, cancel = lifecycle.Cleanup(ctx)
		defer cancel()
	}
	result := agentmcp.DelegateResult{TaskID: tracked.ID, Agent: record.Agent, Node: record.Node, Answer: run.Answer}
	if runErr != nil {
		s.finish(tracked.ID, remoteOutcomeOf(runErr))
	} else if result, runErr = s.land(ctx, parent, tracked, record, d.published); errors.As(runErr, &detached) {
		pending("completion", i18n.DelegateRecoveryTriedCompletion, i18n.DelegateRecoveryProblemCompletion, i18n.DelegateRecoveryReasonCompletion, i18n.DelegateRecoveryAdviceCompletion, runErr)
		return
	}
	if ctx.Err() != nil {
		pending("commit", i18n.DelegateRecoveryTriedCommit, i18n.DelegateRecoveryProblemCommit, i18n.DelegateRecoveryReasonCommit, i18n.DelegateRecoveryAdviceCommit, errors.Join(runErr, ctx.Err()))
		return
	}
	if run.CleanupErr != nil {
		slog.Error(fmt.Sprintf("delegate: retained session cleanup task=%s attempt=%s error=%v", tracked.ID, record.ID, run.CleanupErr), "parent", parent.ID, "node", record.Node)
	}
	s.completeChild(ctx, parent.Channel, parent, tracked, tracked.Goal, entry, result, runErr, run.Last)
}

func (s *Service) finishFromRecord(ctx context.Context, record attempt.Record, outcome task.Outcome) error {
	id := record.TaskID
	if tracked, ok := s.tasks.Get(id); ok {
		for _, row := range tracked.Attempts {
			if row.ExecutionID == record.ID {
				return s.tasks.SettleAttempt(ctx, id, record.ID, record.TurnID, record.EndedAt, outcome, recoveredUsage(record))
			}
		}
	}
	return errors.New("retained delegate receipt has no exact task accounting row")
}

func (s *Service) clearRecovery(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if notice := s.recoveryQuestions[id]; notice != nil && notice.cancel != nil {
		notice.cancel()
	}
	delete(s.recoveryQuestions, id)
}
func (s *Service) reportRecovery(ctx context.Context, binding QuestionBinding, code string, attempted, problem, reason, recommendation i18n.Key) {
	s.mu.Lock()
	if s.recoveryQuestions == nil {
		s.recoveryQuestions = map[string]*recoveryNotice{}
	}
	since := time.Now()
	if old := s.recoveryQuestions[binding.Attempt]; old != nil {
		if old.code == code && old.asked {
			s.mu.Unlock()
			return
		}
		// The child has been out of reach without a break since the
		// first failure, whatever each pass ran into along the way.
		since = old.since
		if old.cancel != nil {
			old.cancel()
		}
	}
	// Inside the quiet stretch the pass is recorded and retried, and the
	// owner hears nothing: joining the child again is this service's work.
	if time.Since(since) < s.RecoveryQuiet {
		s.recoveryQuestions[binding.Attempt] = &recoveryNotice{code: code, since: since}
		s.mu.Unlock()
		slog.Info(fmt.Sprintf("delegate: recovery retrying task=%s attempt=%s node=%s reason=%s", binding.Task, binding.Attempt, binding.Node, code),
			"parent", binding.ParentTask, "conversation", binding.Conversation)
		s.markRecoveryUnsettled(ctx, binding, code, diagnosticOf(s.text, attempted, problem, reason, recommendation))
		return
	}
	questionCtx, cancel := context.WithCancel(s.executions.Detached(ctx))
	notice := &recoveryNotice{code: code, since: since, asked: true, cancel: cancel}
	s.recoveryQuestions[binding.Attempt] = notice
	handler := s.recoveryQuestion
	s.mu.Unlock()
	slog.Warn(fmt.Sprintf("delegate: recovery pending task=%s attempt=%s node=%s reason=%s", binding.Task, binding.Attempt, binding.Node, code),
		"parent", binding.ParentTask, "conversation", binding.Conversation)
	diagnostic := diagnosticOf(s.text, attempted, problem, reason, recommendation)
	s.markRecoveryUnsettled(ctx, binding, code, diagnostic)
	if handler == nil {
		return
	}
	q := RecoveryQuestion{QuestionBinding: binding, Question: view.Question{RequestID: "delegate-recovery/" + binding.Attempt + "/" + code, Kind: "recovery", Title: s.text.T(i18n.DelegateRecoveryTitle), Message: diagnostic, Required: true, AllowFreeText: true, Choices: []view.Choice{{Value: "retry", Label: s.text.T(i18n.RecoveryRetry), Detail: s.text.T(i18n.DelegateRecoveryRetryDetail)}, {Value: "wait", Label: s.text.T(i18n.RecoveryWait), Detail: s.text.T(i18n.DelegateRecoveryWaitDetail)}}}}
	go func() {
		answer, err := handler(questionCtx, q)
		if err != nil {
			s.mu.Lock()
			if s.recoveryQuestions[binding.Attempt] == notice {
				delete(s.recoveryQuestions, binding.Attempt)
			}
			s.mu.Unlock()
			cancel()
			return
		}
		if err == nil && answer.Value == "retry" && questionCtx.Err() == nil {
			s.mu.Lock()
			if s.recoveryQuestions[binding.Attempt] == notice {
				delete(s.recoveryQuestions, binding.Attempt)
			}
			s.mu.Unlock()
			// The answer asked for another pass; nobody is waiting on
			// its outcome, and a pass that fails as a whole is a ledger
			// or shutdown problem, not this child's.
			if err := s.RecoverRetained(questionCtx); err != nil {
				slog.Warn(fmt.Sprintf("delegate: recovery pass after answer: %v", err), "task", binding.Task, "parent", binding.ParentTask, "attempt", binding.Attempt, "conversation", binding.Conversation, "node", binding.Node, "reason", code)
			}
		}
	}()
}

// diagnosticOf is what the owner reads about one way a child could not be
// joined, and what the attempt keeps saying in the ledger.
func diagnosticOf(text i18n.Catalog, attempted, problem, reason, recommendation i18n.Key) string {
	return text.T(i18n.DelegateRecoveryDiagnostic, text.T(attempted), text.T(problem), text.T(reason), text.T(recommendation))
}

// markRecoveryUnsettled records on the attempt why it could not be
// joined, so a child left detached keeps saying so in the ledger.
func (s *Service) markRecoveryUnsettled(ctx context.Context, binding QuestionBinding, code, diagnostic string) {
	current, err := s.attempts.Get(ctx, binding.Attempt)
	if err != nil || current.State.Terminal() {
		return
	}
	if err := s.attempts.MarkUnsettled(ctx, binding.Attempt, "delegate-recovery", errors.New(diagnostic), nil); err != nil {
		slog.Error(fmt.Sprintf("delegate: recovery diagnostic not committed task=%s attempt=%s reason=%s", binding.Task, binding.Attempt, code),
			"parent", binding.ParentTask, "conversation", binding.Conversation, "node", binding.Node, "error", err)
	}
}

func (s *Service) detachChild(spawned task.Task, entry *child, detached *execution.RetainedObserverDetached) {
	if entry.scope != nil {
		entry.scope.Finish(detached)
	}
	s.mu.Lock()
	entry.result = agentmcp.DelegateResult{TaskID: spawned.ID, Agent: spawned.Member, Node: spawned.Node, State: task.StateRunning}
	entry.err = nil
	if s.pending[spawned.ID] == entry {
		delete(s.pending, spawned.ID)
	}
	s.mu.Unlock()
	close(entry.done)
	slog.Info(fmt.Sprintf("delegate: retained observer detached task=%s attempt=%s node=%s", spawned.ID, detached.AttemptID, detached.NodeID), "session", detached.SessionID)
}

// retainedFailure is a node-owned execution's answer kept with its
// failure, so a recovered observer reports it without running the child
// again.
func retainedFailure(record attempt.Record, answer string, cause error) (*attempt.Result, error) {
	result := agentmcp.DelegateResult{TaskID: record.TaskID, Agent: record.Agent, Node: record.Node, Outcome: remoteOutcomeOf(cause), Answer: answer}
	if result.Answer == "" {
		result.Answer = cause.Error()
	}
	output, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return &attempt.Result{Summary: text.Clip(result.Answer, 200), Output: output}, nil
}

func (s *Service) canSettleStopped(ctx context.Context, cause error) bool {
	token := execution.Token(ctx)
	return token != nil && acphost.PromptSettled(cause) && errors.Is(s.tasks.CheckExecution(*token), task.ErrExecutionStopped)
}

// executionBinder is a gate that can bind a recovered child's tool
// authorization to the task epoch, attempt and node session it already
// runs under, so the child keeps its token instead of being minted a
// new one. A gate without it cannot recover a retained child.
type executionBinder interface {
	BindExecution(context.Context, agentmcp.Binding, agentmcp.GrantScope) error
}

func (s *Service) bindDelegatedExecution(ctx context.Context, parent, child task.Task, record attempt.Record) error {
	if s.gate == nil {
		return nil
	}
	binder, ok := s.gate.(executionBinder)
	if !ok || record.Execution == nil {
		return errors.New("delegated tool execution binding is unavailable")
	}
	return binder.BindExecution(ctx, agentmcp.Binding{ConversationID: parent.Channel, AgentID: record.Agent, TaskID: child.ID, DelegatedBy: parent.Member}, agentmcp.GrantScope{TaskID: record.TaskID, TaskEpoch: record.Execution.Epoch, AttemptID: record.ID, ExecutionGeneration: attempt.SessionExecutionEpoch(record), NodeID: record.Node, SessionID: record.Session})
}
