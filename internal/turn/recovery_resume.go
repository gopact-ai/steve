package turn

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

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

// RecoveryBlocked leaves the original attempt and exchange unresolved. The
// question describes reconciliation work; it is not an invented native-agent
// callback and accepting it only retries observation of the retained command.
type RecoveryBlocked struct {
	Question view.Question
	Cause    error
}

func (e *RecoveryBlocked) Error() string { return e.Question.Message }
func (e *RecoveryBlocked) Unwrap() error { return e.Cause }

func retainedBlocked(code, attempted, problem, reason, recommendation string, cause error) *RecoveryBlocked {
	return &RecoveryBlocked{Cause: cause, Question: view.Question{RequestID: "recovery/" + code, Kind: "recovery", Title: "继续任务需要你的处理", Message: "已尝试：" + attempted + "\n\n" + problem + "\n\n" + reason + "\n\n" + recommendation, Required: true, AllowFreeText: true, Choices: []view.Choice{{Value: "retry", Label: "重新检查原执行", Detail: "仅核对节点上的原执行，不会重新发送任务。"}, {Value: "wait", Label: "暂时等待", Detail: "保留当前任务和进度。Steve 会继续自己重连，机器回来后自动接着跑。"}}}}
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
	if c.attempts == nil || c.tasks == nil {
		return nil, errors.New("retained chat recovery is not configured")
	}
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
	if c.attempts == nil || c.tasks == nil {
		return errors.New("retained chat recovery is not configured")
	}
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
	if c.attempts == nil {
		return errors.New("retained probe is not configured")
	}
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
		return Result{}, retainedBlocked("maintenance", "检查当前协调服务", "协调服务暂时不能接续执行。", "服务正在交接或维护。", "建议等待交接完成后重新检查。", nil)
	}
	if c.attempts == nil || c.tasks == nil {
		return Result{}, errors.New("retained chat recovery is not configured")
	}
	record, err := c.retainedChatRecord(parent, id, req)
	if err != nil {
		return Result{}, err
	}
	if delivered, err, ok := c.deliverRetainedChat(parent, req, record); ok {
		return delivered, err
	}
	if record.State != attempt.Running || record.Execution == nil {
		return Result{}, retainedBlocked("state", "检查原执行状态", "原执行目前不能接续。", "它没有保留可接续的运行状态和任务授权。", "建议核对执行记录后继续。", nil)
	}
	if err := c.tasks.CheckExecution(*record.Execution); err != nil {
		return Result{}, retainedBlocked("authorization", "检查任务的当前执行授权", "任务授权已经变化。", "任务可能已被暂停或取消，旧执行不能直接恢复。", "建议先确认任务当前状态。", err)
	}
	manager, ok := c.runtime.(retainedRuntime)
	if !ok {
		return Result{}, retainedBlocked("runtime", "检查节点会话接续接口", "当前服务无法接续节点持有的执行。", "节点会话恢复接口尚未接入。", "建议检查节点服务后重试。", nil)
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	if !turnOwned {
		if !c.beginTurn(req.ConversationID, record.Agent, cancel) {
			return Result{}, retainedBlocked("busy", "检查原会话的执行占用", "这个会话已有另一个执行正在处理。", "不能同时附着多个会话驱动。", "建议等待当前驱动结束后重新检查。", nil)
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
	if c.executions == nil {
		return Result{}, errors.New("retained execution registry is not configured")
	}
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
	spent := &turnSpend{}
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
		return Result{}, retainedBlocked("observer-detached", "接续并观察原执行的进度", "原执行的观察连接再次中断。", "无法确认它是否已经完成，因此保留原执行并等待核实。", "建议恢复连接后重新检查。", runErr)
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
		return c.tasks.SettleAttempt(record.TaskID, record.ID, record.TurnID, time.Now().UTC(), outcome(runErr), task.RecoveryUsage{Tokens: spent.tokens(), Model: spent.model(), Reported: spent.attemptUsage().Reported})
	}
	var tokens task.Tokens
	model := ""
	if record.Usage != nil {
		u := record.Usage
		tokens = task.Tokens{Input: u.Input, Output: u.Output, CachedRead: u.CachedRead, CachedWrite: u.CachedWrite, Total: u.Input + u.Output}
		model = u.Model
	}
	return c.tasks.SettleAttempt(record.TaskID, record.ID, record.TurnID, record.EndedAt, outcome(runErr), task.RecoveryUsage{Tokens: tokens, Model: model, Reported: record.Usage != nil && record.Usage.Reported})
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
		return attempt.Record{}, retainedBlocked("identity", "检查任务和原会话的执行关联", "无法确认这条会话对应的原执行。", "任务、会话或输入标识不一致。", "建议核对原任务记录后继续。", nil)
	}
	if tracked.Requester != "" && tracked.Requester != req.SenderOpenID {
		return attempt.Record{}, errors.New("retained execution belongs to another requester")
	}
	c.rememberMode(req)
	if pendingChatOpen(record) {
		return attempt.Record{}, c.inspectPendingOpen(parent, record)
	}
	if record.State != attempt.Bound && !nodewire.IsManagedSession(record.Session) && !endedBeforeSession(record) {
		return attempt.Record{}, retainedBlocked("native-identity", "检查原节点会话标识", "尚未取得可接续的原生会话。", "恢复准备可能在建立会话前中断。", "建议重新检查已确认的恢复准备。", nil)
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
			return Result{}, retainedBlocked("result", "读取原执行的已提交结果", "原执行已经结束，但完整回复记录不可用。", "不能为了补回复而重新执行任务。", "建议检查已保存的产物与执行记录。", nil), true
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
		return Result{}, retainedBlocked("terminal-unsettled", "检查执行结束记录和停止证据", "原执行的停止状态仍未确认。", "执行记录已经结束，但缺少原节点的明确结算或停止证明。", "建议核对原进程和外部操作后再决定恢复方式。", harness.ErrStopUnconfirmed), true
	}
	if !record.State.Terminal() {
		return Result{}, nil, false
	}
	if record.Error == "" {
		return Result{}, retainedBlocked("terminal", "读取已结束的执行记录", "原执行已经结束，但没有可以补投的完整结果。", "重新发送任务可能重复已执行的操作。", "建议核对原执行记录后决定后续工作。", nil), true
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
		return nil, nodewire.SessionState{}, record, retainedBlocked("node-attach", "使用原任务和会话标识连接执行节点", "暂时无法接回原执行。", "节点可能离线，或原生会话已不可用。已有执行不会被重放。", "建议恢复原节点连接后重新检查；仍无法解决时保留此任务等待处理。", err)
	}
	inspector, ok := runner.(harness.RetainedSessionInspector)
	if !ok {
		return nil, nodewire.SessionState{}, record, errors.New("retained runner cannot inspect its command binding")
	}
	observed, err := inspector.InspectRetained(ctx)
	if err != nil {
		return nil, nodewire.SessionState{}, record, retainedBlocked("node-state", "读取节点持有的输入回执和执行状态", "暂时无法核实原执行状态。", "连接失败不能证明原执行停止。", "建议等原节点恢复后重新检查。", err)
	}
	if observed.Command != nil && observed.Command.State == nodewire.SessionCommandUncertain {
		return nil, nodewire.SessionState{}, record, retainedBlocked("native-interrupted", "节点返回原输入回执和中断记录", "原执行已经没有可直接接回的运行现场。", "节点服务可能重启过，部分外部操作的结果仍未确认。", "建议核对已完成操作与保存的检查点；确认恢复方式前，原任务保持待处理。", harness.ErrStopUnconfirmed)
	}
	refreshed, err := c.attempts.RecoverRetained(ctx, record.ID, attempt.RetainedEvidence{ObservedAt: time.Now(), Session: observed})
	if err != nil {
		reason := "节点返回的会话、任务或输入标识与已提交的执行记录不一致。"
		switch {
		case errors.Is(err, task.ErrExecutionStopped):
			reason = "用户对任务的暂停、取消或授权变更已经撤销原执行权限。"
		case errors.Is(err, ledger.ErrStale):
			reason = "原写入租约已经变更持有者或代际，不能继续使用旧执行的写入权限。"
		case errors.Is(err, ledger.ErrConflict):
			reason = "核对期间协调账本发生变化，需要重新读取最新记录。"
		}
		return nil, nodewire.SessionState{}, record, retainedBlocked("retained-evidence", "核对节点输入回执、任务授权及原有写入租约", "原执行暂时不能安全接续。", reason, "建议核对原节点与执行记录，确认后再继续。", err)
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
