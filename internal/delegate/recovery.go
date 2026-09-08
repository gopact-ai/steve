package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/lifecycle"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/task"
	"github.com/gopact-ai/steve/internal/view"
)

// QuestionBinding names the existing child execution that originated a native
// question. Recovery diagnostics use a separate callback and never approve it.
type QuestionBinding struct {
	Conversation, ParentTask, Task, Attempt, Node, Agent, Project, Session string
}

type RecoveryQuestion struct {
	QuestionBinding
	Question view.Question
}

type recoveryNotice struct {
	code   string
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

func (s *Service) SetRetainedQuestionHandlers(
	ask func(context.Context, QuestionBinding, permission.Ask) (acp.RequestPermissionOutcome, error),
	askUser func(context.Context, QuestionBinding, view.Question) (view.Answer, error),
) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retainedPermission, s.retainedQuestion = ask, askUser
}

func questionBinding(parent, child task.Task, record attempt.Record) QuestionBinding {
	return QuestionBinding{Conversation: parent.Channel, ParentTask: parent.ID, Task: child.ID, Attempt: record.ID, Node: record.Node, Agent: record.Agent, Project: record.Project, Session: record.Session}
}

func (s *Service) nodeQuestionHandlers(binding QuestionBinding) (permission.AskFunc, acphost.AskUserFunc) {
	s.mu.Lock()
	ask, askUser := s.retainedPermission, s.retainedQuestion
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
	live, err := s.attempts.Live(ctx)
	if err != nil {
		return err
	}
	closed, err := s.attempts.Closed(ctx)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, record := range append(live, closed...) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if seen[record.ID] || record.Kind != attempt.KindDelegate || (!lifecycle.IsManaged(record.Session) && !pendingDelegatePreparation(record)) {
			continue
		}
		seen[record.ID] = true
		tracked, ok := s.tasks.Get(record.TaskID)
		if !ok || !tracked.Delegated() || tracked.Parent == "" || (tracked.Result != nil && tracked.Finished() && len(tracked.Attempts) > 0 && !tracked.Attempts[len(tracked.Attempts)-1].Open()) {
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
	return nil
}

// recoveryPending reports one way a child could not be joined: a question
// to the parent's conversation, and the child left detached.
type recoveryPending func(code, attempted, problem, reason, recommendation string, cause error)

// recoverChild joins a delegated child's node-owned execution again. What
// the attempt already committed is delivered from the record; a running
// one is attached, reconciled against the node's evidence, and reattached
// from the prompt on. Every way it cannot be joined is a recovery question
// to the parent, never a second prompt.
func (s *Service) recoverChild(ctx context.Context, parent, tracked task.Task, record attempt.Record, entry *child) {
	binding := questionBinding(parent, tracked, record)
	pending := func(code, attempted, problem, reason, recommendation string, cause error) {
		s.reportRecovery(ctx, binding, code, attempted, problem, reason, recommendation)
		s.completeChild(ctx, parent.Channel, parent, tracked, tracked.Goal, entry, entry.result, retainedDetached(record, cause), view.Progress{})
	}
	if record.Execution == nil || record.Execution.TaskID != tracked.ID || record.Project != tracked.ProjectID || record.Agent != tracked.Member || record.Node != tracked.Node {
		pending("identity", "核对已提交的子任务与执行身份", "无法确认原子任务的执行归属。", "任务、节点或授权标识不一致，不能从该会话继续提交结果。", "建议核对原任务记录后重新检查。", nil)
		return
	}
	scope, err := s.executions.BeginAccepted(ctx, execution.Key{TaskID: tracked.ID, InstanceID: record.TurnID, AttemptID: record.ID}, record.Execution)
	if err != nil {
		pending("authorization", "核对子任务的执行授权", "当前不能接续这个子任务。", "原任务或其父任务的授权可能已经改变。", "建议确认任务状态后重新检查。", err)
		return
	}
	entry.scope = scope
	ctx = scope.Context()
	if s.deliverRecovered(ctx, parent, tracked, record, entry, pending) {
		return
	}
	runner, ask, askUser, ok := s.attachChild(ctx, parent, tracked, &record, scope, binding, pending)
	if !ok {
		return
	}
	s.clearRecovery(record.ID)
	d := &delegation{service: s, parent: parent, child: tracked, at: harness.Placement{Node: record.Node, Harness: record.Harness}, binding: binding}
	run, runErr := lifecycle.Reattach(ctx, lifecycle.Options{
		Attempts: s.attempts, Roster: s.roster, Sessions: s.sessions, Workspaces: s.artifacts, Actor: "delegate-recovery",
		Spec: record.Spec, At: d.at, Resume: true, Ask: ask, AskUser: askUser,
		Observe: func(progress view.Progress) {
			s.report(Child{Conversation: parent.Channel, ParentTask: parent.ID, Task: tracked.ID, Agent: record.Agent, Node: record.Node, Goal: tracked.Goal, State: task.StateRunning, Since: record.StartedAt, Elapsed: time.Since(record.StartedAt), Attempt: record.ID}, progress)
		},
		Finish: d.finish, Failed: d.failed,
		// The node keeps the session. An observer that cannot vouch for
		// the end, or was cancelled, leaves the record as it is and asks:
		// the question is what quarantines it.
		Settlement: lifecycle.Settlement{Quarantine: lifecycle.QuarantineManaged, DetachManaged: true, Detachment: lifecycle.DetachSilently, CancelDetaches: true},
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
			pending("result", "读取原子任务已提交的完整结果", "原执行已完成，但完整回复记录不可用。", "重新执行可能重复已完成的操作。", "建议核对已保存的产物和原执行记录。", nil)
			return true
		}
		s.clearRecovery(record.ID)
		if record.Result.Artifact != "" && hasArtifactRef(result.Refs, record.Result.Artifact) {
			if err := s.artifacts.Defer(ctx, record.Project, record.Result.Artifact, "task #"+tracked.ID, artifact.SourceOf(ctx, record.ID)...); err != nil {
				pending("landing", "恢复已提交产物的落地记录", "子任务的回复已保存，但产物落地尚未恢复。", "项目可能暂时不可用，不能声称文件已经落地。", "建议恢复项目连接后重新检查。", err)
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
				pending("failed-output", "读取已结束子任务的完整答复", "执行已结束，但答复记录不完整。", "不能通过重新执行来补一份失败答复。", "建议核对原执行记录后继续。", nil)
				return true
			}
		}
		s.completeChild(ctx, parent.Channel, parent, tracked, tracked.Goal, entry, result, errors.New(record.Error), view.Progress{})
		return true
	}
	if record.State != attempt.Running {
		pending("state", "核对原子任务的持久执行阶段", "原执行目前不能直接接续。", "它没有停留在可核对的运行阶段，或仍缺少明确的停止证据。", "建议检查原节点、产物与执行记录后再决定恢复方式。", nil)
		return true
	}
	return false
}

// attachChild joins the child's node-owned session: the node's evidence is
// checked against the record in one ledger transaction, the child's tool
// authorization is bound again, and its pending questions must have
// someone to answer them before the observer takes over.
func (s *Service) attachChild(ctx context.Context, parent, tracked task.Task, record *attempt.Record, scope *execution.Scope, binding QuestionBinding, pending recoveryPending) (harness.ResumableRunner, permission.AskFunc, acphost.AskUserFunc, bool) {
	manager, ok := s.sessions.(retainedRuntime)
	if !ok {
		pending("runtime", "检查保留会话的接续接口", "当前服务无法接回原执行。", "节点会话接续接口不可用。", "建议更新并恢复原节点连接后重试。", nil)
		return nil, nil, nil, false
	}
	runner, err := manager.AttachRetainedSession(ctx, harness.Placement{Node: record.Node, Harness: record.Harness}, record.Session, record.Workspace.Path)
	if err != nil {
		pending("attach", "按原任务和会话标识连接执行节点", "暂时无法接回原子任务。", "节点可能离线；连接失败不能证明原执行已停止。", "建议恢复该节点连接后重新检查，保留原任务和已有进度。", err)
		return nil, nil, nil, false
	}
	inspector, ok := runner.(harness.RetainedSessionInspector)
	if !ok {
		pending("evidence", "读取原节点的执行回执", "节点没有提供可验证的执行状态。", "没有绑定及输入回执就不能继续结算该执行。", "建议核对节点服务后重新检查。", nil)
		return nil, nil, nil, false
	}
	state, err := inspector.InspectRetained(ctx)
	if err != nil {
		pending("inspect", "读取原节点持有的命令与执行状态", "暂时无法核实原子任务。", "原节点当前不可达，执行不能被重放。", "建议恢复原节点后重新检查。", err)
		return nil, nil, nil, false
	}
	recovered, err := s.attempts.RecoverRetained(ctx, record.ID, attempt.RetainedEvidence{ObservedAt: time.Now(), Session: state})
	if err != nil {
		pending("reconcile", "核对原命令回执、任务授权和同一持有者的写入租约", "原子任务暂时不能安全接续。", "节点证据不匹配、原授权已改变，或原生执行状态仍不确定。", "建议核对原节点和执行记录，明确后再继续。", err)
		return nil, nil, nil, false
	}
	*record = recovered
	if err := s.bindDelegatedExecution(ctx, parent, tracked, *record); err != nil {
		pending("messaging", "核对原子任务的工具授权", "原工具授权暂时无法恢复。", "只能使用同一任务和原生会话的授权，不能悄悄换一个工具 token。", "建议恢复原协作服务后重新检查。", err)
		return nil, nil, nil, false
	}
	scope.AdoptRetained()
	ask, askUser := s.nodeQuestionHandlers(binding)
	for _, q := range state.Questions {
		if q.State == "pending" && ((q.Permission != nil && ask == nil) || (q.Permission == nil && askUser == nil)) {
			pending("question", "读取原节点保留的待答问题", "原子任务正在等待一个真实的 Agent 问题。", "当前服务尚未接通该原生问答，平台恢复问题不能替代工具授权。", "建议接通原会话问答后继续，原执行会保持等待。", nil)
			return nil, nil, nil, false
		}
	}
	return runner, ask, askUser, true
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
			pending("settlement", "保存节点的原命令结算回执", "节点已给出结果，但协调记录尚未保存。", "保留同一命令，避免重新执行。", "建议恢复协调服务后重新核对。", runErr)
		case step != nil && step.Step == lifecycle.StepFinish && !finished:
			s.spent(tracked.ID, run.Last)
			pending("failed-result", "保存原执行的错误与完整答复", "失败结果尚未完整保存。", "原命令已经结束，但不能提前向父任务结算。", "建议恢复存储后重新核对原结果。", runErr)
		case step != nil && step.Step == lifecycle.StepFinish:
			s.spent(tracked.ID, run.Last)
			pending("completion", "保存原子任务的产物和结算记录", "原命令已经返回，但产物或结果尚未完整提交。", "节点或存储可能暂时不可用，不能重复执行原任务来补结果。", "建议恢复节点与存储后重新核对原命令和产物。", runErr)
		default:
			pending("observer", "接续并观察原子任务的进度", "观察连接再次中断。", "尚未确认原执行的最终结果，原任务和预算保持未决。", "建议恢复连接后重新检查同一次执行。", errors.Join(runErr, ctx.Err()))
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
		s.finish(tracked.ID, outcomeOf(runErr))
	} else if result, runErr = s.land(ctx, parent, tracked, record, d.published); errors.As(runErr, &detached) {
		pending("completion", "保存原子任务的产物和结算记录", "原命令已经返回，但产物或结果尚未完整提交。", "节点或存储可能暂时不可用，不能重复执行原任务来补结果。", "建议恢复节点与存储后重新核对原命令和产物。", runErr)
		return
	}
	if ctx.Err() != nil {
		pending("commit", "保存原子任务的执行结果", "协调连接在结果保存期间中断。", "已保存的节点结果仍属于原命令，不能重发任务。", "建议恢复连接后核对同一次执行。", errors.Join(runErr, ctx.Err()))
		return
	}
	if run.CleanupErr != nil {
		log.Printf("delegate: retained session cleanup task=%s attempt=%s error=%v", tracked.ID, record.ID, run.CleanupErr)
	}
	s.completeChild(ctx, parent.Channel, parent, tracked, tracked.Goal, entry, result, runErr, run.Last)
}

func (s *Service) finishFromRecord(record attempt.Record, outcome task.Outcome) error {
	id, usage := record.TaskID, record.Usage
	if tracked, ok := s.tasks.Get(id); ok {
		for _, row := range tracked.Attempts {
			if row.ExecutionID == record.ID {
				var spent task.RecoveryUsage
				if usage != nil {
					spent = task.RecoveryUsage{Tokens: task.FromUsage(uint64(max(usage.Input, 0)), uint64(max(usage.Output, 0)), uint64(max(usage.CachedRead, 0)), uint64(max(usage.CachedWrite, 0))), Model: usage.Model, Reported: usage.Reported}
				}
				return s.tasks.SettleAttempt(id, record.ID, record.TurnID, record.EndedAt, outcome, spent)
			}
		}
	}
	return errors.New("retained delegate receipt has no exact task accounting row")
}
func hasArtifactRef(refs []string, id string) bool {
	for _, ref := range refs {
		if ref == "artifact "+id {
			return true
		}
	}
	return false
}

func (s *Service) clearRecovery(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if notice := s.recoveryQuestions[id]; notice != nil && notice.cancel != nil {
		notice.cancel()
	}
	delete(s.recoveryQuestions, id)
}
func (s *Service) reportRecovery(ctx context.Context, binding QuestionBinding, code, attempted, problem, reason, recommendation string) {
	s.mu.Lock()
	if s.recoveryQuestions == nil {
		s.recoveryQuestions = map[string]*recoveryNotice{}
	}
	if old := s.recoveryQuestions[binding.Attempt]; old != nil {
		if old.code == code {
			s.mu.Unlock()
			return
		}
		if old.cancel != nil {
			old.cancel()
		}
	}
	questionCtx, cancel := context.WithCancel(s.executions.Detached(ctx))
	notice := &recoveryNotice{code: code, cancel: cancel}
	s.recoveryQuestions[binding.Attempt] = notice
	handler := s.recoveryQuestion
	s.mu.Unlock()
	log.Printf("delegate: recovery pending task=%s attempt=%s node=%s reason=%s", binding.Task, binding.Attempt, binding.Node, code)
	diagnostic := fmt.Sprintf("已尝试：%s。\n\n%s\n\n%s\n\n%s", attempted, problem, reason, recommendation)
	if current, err := s.attempts.Get(ctx, binding.Attempt); err == nil && !current.State.Terminal() {
		if err := s.attempts.MarkUnsettled(ctx, binding.Attempt, "delegate-recovery", errors.New(diagnostic), nil); err != nil {
			log.Printf("delegate: recovery diagnostic not committed task=%s attempt=%s reason=%s", binding.Task, binding.Attempt, code)
		}
	}
	if handler == nil {
		return
	}
	q := RecoveryQuestion{QuestionBinding: binding, Question: view.Question{RequestID: "delegate-recovery/" + binding.Attempt + "/" + code, Kind: "recovery", Title: "子任务继续执行需要你的处理", Message: diagnostic, Required: true, AllowFreeText: true, Choices: []view.Choice{{Value: "retry", Label: "重新检查原执行", Detail: "核对原节点和命令，不重新发送任务。"}, {Value: "wait", Label: "暂时等待", Detail: "保留子任务和进度，等待节点恢复。"}}}}
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
			_ = s.RecoverRetained(questionCtx)
		}
	}()
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
	log.Printf("delegate: retained observer detached task=%s attempt=%s node=%s", spawned.ID, detached.AttemptID, detached.NodeID)
}

// retainedFailure is a node-owned execution's answer kept with its
// failure, so a recovered observer reports it without running the child
// again.
func retainedFailure(record attempt.Record, answer string, cause error) (*attempt.Result, error) {
	result := agentmcp.DelegateResult{TaskID: record.TaskID, Agent: record.Agent, Node: record.Node, Outcome: outcomeOf(cause), Answer: answer}
	if result.Answer == "" {
		result.Answer = cause.Error()
	}
	output, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return &attempt.Result{Summary: clipRunes(result.Answer, 200), Output: output}, nil
}

func (s *Service) failRetainedResult(ctx context.Context, record attempt.Record, result agentmcp.DelegateResult, cause error, usage *attempt.Usage) (attempt.Record, error) {
	failed, err := retainedFailure(record, result.Answer, cause)
	if err != nil {
		return attempt.Record{}, err
	}
	return s.attempts.Advance(ctx, record.ID, attempt.Failed, "delegate", func(r *attempt.Record) {
		r.Error = cause.Error()
		r.Usage = usage
		r.Result = failed
	})
}

func (s *Service) canSettleStopped(ctx context.Context, cause error) bool {
	token := execution.Token(ctx)
	return token != nil && acphost.PromptSettled(cause) && errors.Is(s.tasks.CheckExecution(*token), task.ErrExecutionStopped)
}

func (s *Service) bindDelegatedExecution(ctx context.Context, parent, child task.Task, record attempt.Record) error {
	if s.gate == nil {
		return nil
	}
	binder, ok := s.gate.(interface {
		BindExecution(context.Context, agentmcp.Binding, agentmcp.GrantScope) error
	})
	if !ok || record.Execution == nil {
		return errors.New("delegated tool execution binding is unavailable")
	}
	return binder.BindExecution(ctx, agentmcp.Binding{ConversationID: parent.Channel, AgentID: record.Agent, TaskID: child.ID, DelegatedBy: parent.Member}, agentmcp.GrantScope{TaskID: record.TaskID, TaskEpoch: record.Execution.Epoch, AttemptID: record.ID, ExecutionGeneration: attempt.SessionExecutionEpoch(record), NodeID: record.Node, SessionID: record.Session})
}
