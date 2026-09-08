package turn

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/ledger"
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
	return &RecoveryBlocked{Cause: cause, Question: view.Question{RequestID: "recovery/" + code, Kind: "recovery", Title: "继续任务需要你的处理", Message: "已尝试：" + attempted + "\n\n" + problem + "\n\n" + reason + "\n\n" + recommendation, Required: true, AllowFreeText: true, Choices: []view.Choice{{Value: "retry", Label: "重新检查原执行", Detail: "仅核对节点上的原执行，不会重新发送任务。"}, {Value: "wait", Label: "暂时等待", Detail: "保留当前任务和进度，等机器恢复后再处理。"}}}}
}

// RetainedChats identifies accepted native commands and committed but not yet
// delivered results. It creates neither tasks nor replacement attempts.
func (c *Coordinator) RetainedChats(ctx context.Context) ([]RetainedChat, error) {
	if c.attempts == nil || c.tasks == nil {
		return nil, errors.New("retained chat recovery is not configured")
	}
	live, err := c.attempts.Live(ctx)
	if err != nil {
		return nil, err
	}
	closed, err := c.attempts.Closed(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var result []RetainedChat
	for _, r := range append(live, closed...) {
		if seen[r.ID] || r.Kind != attempt.KindChat || r.State == attempt.Superseded || (!strings.HasPrefix(r.Session, "ns_") && !attempt.Relocatable(r) && !attempt.PreparingRelocation(r) && !pendingChatOpen(r)) || (r.State != attempt.Running && !r.State.Terminal() && !attempt.Relocatable(r) && !attempt.PreparingRelocation(r) && !pendingChatOpen(r)) {
			continue
		}
		seen[r.ID] = true
		tracked, ok := c.tasks.Get(r.TaskID)
		if !ok {
			continue
		}
		result = append(result, RetainedChat{AttemptID: r.ID, TaskID: r.TaskID, Conversation: tracked.Channel, MessageID: r.TurnID, AgentID: r.Agent, NodeID: r.Node, ProjectID: r.Project, Completed: r.State.Terminal()})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].AttemptID < result[j].AttemptID })
	return result, nil
}

type retainedRuntime interface {
	AttachRetainedSession(context.Context, harness.Placement, string, string) (harness.ResumableRunner, error)
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
	record, err := c.attempts.Get(parent, id)
	if err != nil {
		return Result{}, err
	}
	tracked, ok := c.tasks.Get(record.TaskID)
	if !ok || record.Kind != attempt.KindChat || tracked.Channel != req.ConversationID || record.TurnID != req.MessageID {
		return Result{}, retainedBlocked("identity", "检查任务和原会话的执行关联", "无法确认这条会话对应的原执行。", "任务、会话或输入标识不一致。", "建议核对原任务记录后继续。", nil)
	}
	if tracked.Requester != "" && tracked.Requester != req.SenderOpenID {
		return Result{}, errors.New("retained execution belongs to another requester")
	}
	c.rememberMode(req)
	if pendingChatOpen(record) {
		return Result{}, c.inspectPendingOpen(parent, record)
	}
	if !strings.HasPrefix(record.Session, "ns_") {
		return Result{}, retainedBlocked("native-identity", "检查原节点会话标识", "尚未取得可接续的原生会话。", "恢复准备可能在建立会话前中断。", "建议重新检查已确认的恢复准备。", nil)
	}
	if record.State == attempt.Bound {
		if record.Result == nil || len(record.Result.Output) == 0 || json.Unmarshal(record.Result.Output, &result) != nil {
			return Result{}, retainedBlocked("result", "读取原执行的已提交结果", "原执行已经结束，但完整回复记录不可用。", "不能为了补回复而重新执行任务。", "建议检查已保存的产物与执行记录。", nil)
		}
		result.Attempt = record.ID
		if err := c.finishRetainedTask(record, nil, nil); err != nil {
			return Result{}, err
		}
		return c.gateDisclosure(parent, req, result)
	}
	if record.State.Terminal() && record.Unsettled {
		return Result{}, retainedBlocked("terminal-unsettled", "检查执行结束记录和停止证据", "原执行的停止状态仍未确认。", "执行记录已经结束，但缺少原节点的明确结算或停止证明。", "建议核对原进程和外部操作后再决定恢复方式。", harness.ErrStopUnconfirmed)
	}
	if record.State.Terminal() {
		// An already committed failure/cancellation is a delivery fact. It
		// cannot authorize replaying the original native prompt.
		if record.Error == "" {
			return Result{}, retainedBlocked("terminal", "读取已结束的执行记录", "原执行已经结束，但没有可以补投的完整结果。", "重新发送任务可能重复已执行的操作。", "建议核对原执行记录后决定后续工作。", nil)
		}
		result = Result{AgentID: record.Agent, Text: record.Error, Attempt: record.ID}
		failure := errors.New(record.Error)
		if err := c.finishRetainedTask(record, failure, nil); err != nil {
			return Result{}, err
		}
		return result, failure
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
	knownRetained := record.Node != "" && strings.HasPrefix(record.Session, "ns_")
	settled := false
	var cleanupFailure error
	defer func() {
		var unresolved error
		if cleanupFailure != nil {
			unresolved = cleanupFailure
			if knownRetained && ctx.Err() != nil {
				unresolved = &execution.RetainedObserverDetached{AttemptID: record.ID, NodeID: record.Node, SessionID: record.Session, Cause: errors.Join(cleanupFailure, ctx.Err())}
			}
		} else if !settled && err != nil {
			unresolved = errors.Join(harness.ErrStopUnconfirmed, err)
			if knownRetained {
				unresolved = &execution.RetainedObserverDetached{AttemptID: record.ID, NodeID: record.Node, SessionID: record.Session, Cause: unresolved}
			}
		}
		scope.Finish(unresolved)
	}()
	ctx = scope.Context()
	if req.OnTurnReady != nil {
		req.OnTurnReady(record.TaskID, record.ID)
	}
	runner, err := manager.AttachRetainedSession(ctx, harness.Placement{Node: record.Node, Harness: record.Harness}, record.Session, record.Workspace.Path)
	if err != nil {
		return Result{}, retainedBlocked("node-attach", "使用原任务和会话标识连接执行节点", "暂时无法接回原执行。", "节点可能离线，或原生会话已不可用。已有执行不会被重放。", "建议恢复原节点连接后重新检查；仍无法解决时保留此任务等待处理。", err)
	}
	inspector, ok := runner.(harness.RetainedSessionInspector)
	if !ok {
		return Result{}, errors.New("retained runner cannot inspect its command binding")
	}
	observed, err := inspector.InspectRetained(ctx)
	if err != nil {
		return Result{}, retainedBlocked("node-state", "读取节点持有的输入回执和执行状态", "暂时无法核实原执行状态。", "连接失败不能证明原执行停止。", "建议等原节点恢复后重新检查。", err)
	}
	if observed.Command != nil && observed.Command.State == nodewire.SessionCommandUncertain {
		return Result{}, retainedBlocked("native-interrupted", "节点返回原输入回执和中断记录", "原执行已经没有可直接接回的运行现场。", "节点服务可能重启过，部分外部操作的结果仍未确认。", "建议核对已完成操作与保存的检查点；确认恢复方式前，原任务保持待处理。", harness.ErrStopUnconfirmed)
	}
	record, err = c.attempts.RecoverRetained(ctx, record.ID, attempt.RetainedEvidence{ObservedAt: time.Now(), Session: observed})
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
		return Result{}, retainedBlocked("retained-evidence", "核对节点输入回执、任务授权及原有写入租约", "原执行暂时不能安全接续。", reason, "建议核对原节点与执行记录，确认后再继续。", err)
	}
	knownRetained = true
	if err := c.bindExecutionGate(ctx, req.ConversationID, record.ID); err != nil {
		return Result{}, err
	}
	settled = observed.Command != nil && observed.Command.Settled && (observed.Command.State == nodewire.SessionCommandCompleted || observed.Command.State == nodewire.SessionCommandCancelled)
	scope.AdoptRetained()
	c.setRunner(req.ConversationID, record.Agent, runner)
	beat, stop := context.WithCancel(ctx)
	defer stop()
	lost := c.attempts.Heartbeat(beat, record.ID)
	go func() {
		select {
		case <-lost:
			cancel()
		case <-beat.Done():
		}
	}()
	spent := &turnSpend{}
	progress := spent.wrap(req.OnProgress, record.Agent)
	req.phase(view.PhaseRunning)
	out, activity, runErr := runner.ResumeTurn(ctx, req.OnAsk, req.OnAskUser, progress)
	if errors.Is(runErr, harness.ErrStopUnconfirmed) || (ctx.Err() != nil && !acphost.PromptSettled(runErr)) {
		if ctx.Err() == nil {
			_ = c.attempts.MarkUnsettled(ctx, record.ID, "retained-session", runErr, spent.attemptUsage())
		}
		return Result{}, retainedBlocked("observer-detached", "接续并观察原执行的进度", "原执行的观察连接再次中断。", "无法确认它是否已经完成，因此保留原执行并等待核实。", "建议恢复连接后重新检查。", runErr)
	}
	settled = acphost.PromptSettled(runErr)
	if settled {
		req.phase(view.PhaseFinishing)
	}
	result = Result{AgentID: record.Agent, Text: out, Activity: activity, Attempt: record.ID, Injected: &Injected{Project: record.Project, Workspace: record.Workspace.Path, Agent: record.Agent, Node: record.Node, Harness: record.Harness, Session: record.Session}}
	if settled {
		runErr, cleanupFailure = c.settleRetained(ctx, record, result, runErr, spent)
	} else {
		cleanupFailure = c.closeAttempt(ctx, record.ID, result, runErr, spent, nil)
	}
	if cleanupFailure != nil {
		return Result{}, cleanupFailure
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
