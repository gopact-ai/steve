package agentexec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/lifecycle"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/view"
)

// RecoveryBlocked leaves the original attempt and input in place. Its question
// is a platform diagnostic, never an answer to a native permission request.
type RecoveryBlocked struct {
	AttemptID, TaskID string
	Question          view.Question
	Cause             error
}

func (e *RecoveryBlocked) Error() string            { return e.Question.Message }
func (e *RecoveryBlocked) Unwrap() error            { return e.Cause }
func (e *RecoveryBlocked) UnsettledAttempt() string { return e.AttemptID }
func Blocked(record attempt.Record, code, attempted, problem, recommendation string, cause error) *RecoveryBlocked {
	if cause != nil {
		problem += "\n诊断信息：" + cause.Error()
	}
	return &RecoveryBlocked{AttemptID: record.ID, TaskID: record.TaskID, Cause: cause, Question: view.Question{RequestID: "execution-recovery/" + record.ID + "/" + code, Kind: "recovery", Title: "继续执行需要你的处理", Message: "已尝试：" + attempted + "。\n\n" + problem + "\n\n原执行状态尚未完整确认，不能重新发送原任务。\n\n" + recommendation, Required: true, AllowFreeText: true, Choices: []view.Choice{{Value: "retry", Label: "重新检查原执行", Detail: "核对原节点、命令和已保存结果。"}, {Value: "wait", Label: "暂时等待", Detail: "保留当前任务与进度，等条件恢复。"}}}}
}

type auxiliaryInput struct {
	Spec   Spec   `json:"spec"`
	Prompt string `json:"prompt"`
}
type auxiliaryOutput struct {
	Input      *auxiliaryInput `json:"input,omitempty"`
	WorkID     string          `json:"work_id"`
	Answer     string          `json:"answer"`
	Error      string          `json:"error,omitempty"`
	Validation bool            `json:"validation,omitempty"`
}
type retainedSessions interface {
	AttachRetainedSession(context.Context, harness.Placement, string, string) (harness.ResumableRunner, error)
}

func workID(spec Spec, prompt string) (string, error) {
	raw, err := json.Marshal(struct {
		Task, Turn, Agent, Project, Base, Kind, Prompt string
		Source                                         json.RawMessage
	}{spec.TaskID, spec.TurnID, spec.Agent, spec.Project, spec.Base, string(spec.Kind), prompt, spec.Source})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
func auxiliaryKey(spec Spec) string {
	return spec.TaskID + "\x00" + string(spec.Kind) + "\x00" + spec.TurnID
}
func (r *Runner) claim(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active == nil {
		r.active = map[string]bool{}
	}
	if r.active[key] {
		return false
	}
	r.active[key] = true
	return true
}
func (r *Runner) release(key string) { r.mu.Lock(); delete(r.active, key); r.mu.Unlock() }
func (r *Runner) SetQuestionHandlers(ask permission.AskFunc, askUser acphost.AskUserFunc) {
	r.mu.Lock()
	r.ask, r.askUser = ask, askUser
	r.mu.Unlock()
}
func (r *Runner) SetObserver(observe func(attempt.Record, view.Progress)) {
	r.mu.Lock()
	r.observe = observe
	r.mu.Unlock()
}
func (r *Runner) callbacks() (permission.AskFunc, acphost.AskUserFunc, func(attempt.Record, view.Progress)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ask, r.askUser, r.observe
}

// ResumeAttempt consumes only the native command already linked to this
// attempt. Its caller validates the original task/exchange identity first.
func (r *Runner) ResumeAttempt(ctx context.Context, id string, validate func(string) error) (Result, error) {
	record, err := r.attempts.Get(ctx, id)
	if err != nil {
		return Result{}, err
	}
	spec := Spec{TaskID: record.TaskID, TurnID: record.TurnID, Kind: record.Kind}
	key := auxiliaryKey(spec)
	if !r.claim(key) {
		return Result{}, Blocked(record, "busy", "检查原执行的观察者", "原执行已有一个观察者正在处理。", "建议等待这次观察完成后重新检查。", nil)
	}
	defer r.release(key)
	return r.resumeAttempt(ctx, record, validate)
}
func (r *Runner) resumeAttempt(parent context.Context, record attempt.Record, validate func(string) error) (out Result, runErr error) {
	out.Attempt = record
	if _, err := originalInput(record); err != nil {
		return out, Blocked(record, "input", "读取原执行的请求与工作身份", "原请求记录不完整或无法核对。", "建议核对原任务和执行记录，不构造另一份原始请求。", err)
	}
	if (record.Kind != attempt.KindPlan && record.Kind != attempt.KindVerify) || !strings.HasPrefix(record.Session, "ns_") || record.Execution == nil || record.WorkID == "" {
		return out, Blocked(record, "identity", "检查原规划或验证执行的身份", "原执行缺少稳定的工作或原生会话标识。", "建议核对任务记录后继续。", nil)
	}
	scope, err := r.executions.BeginAccepted(parent, execution.Key{TaskID: record.TaskID, InstanceID: record.TurnID, AttemptID: record.ID}, record.Execution)
	if err != nil {
		return out, err
	}
	var unresolved error
	defer func() { scope.Finish(unresolved) }()
	ctx := scope.Context()
	if record.State.Terminal() {
		if record.Unsettled {
			return out, Blocked(record, "unsettled", "检查已结束执行的停止记录", "原执行的停止状态仍未确认。", "建议核对原节点与外部操作后继续。", harness.ErrStopUnconfirmed)
		}
		if err := SettleBudget(r.budget, record, nil); err != nil {
			return out, Blocked(record, "accounting", "核对原执行的持久预算记录", "原执行已结束，但预算结算尚未完成。", "建议恢复存储后核对同一次执行。", err)
		}
		if err := r.cleanupAuxiliary(ctx, record); err != nil {
			return out, Blocked(record, "cleanup", "释放已结束执行的工作区", "执行结果已保存，但原工作区尚未释放。", "建议恢复节点连接后重新检查。", err)
		}
		return decodeAuxiliary(record, validate)
	}
	retain := func(code string, cause error) (Result, error) {
		unresolved = &execution.RetainedObserverDetached{AttemptID: record.ID, NodeID: record.Node, SessionID: record.Session, Cause: errors.Join(harness.ErrStopUnconfirmed, cause)}
		return out, Blocked(record, code, "按原任务、会话和命令标识核对执行", "原规划或验证执行暂时不能安全接续。", "建议恢复原节点或存储后重新检查。", unresolved)
	}
	manager, ok := r.sessions.(retainedSessions)
	if !ok {
		return retain("runtime", errors.New("retained session interface unavailable"))
	}
	session, err := manager.AttachRetainedSession(ctx, harness.Placement{Node: record.Node, Harness: record.Harness}, record.Session, record.Workspace.Path)
	if err != nil {
		return retain("attach", err)
	}
	inspector, ok := session.(harness.RetainedSessionInspector)
	if !ok {
		return retain("inspect", errors.New("retained state inspection unavailable"))
	}
	observed, err := inspector.InspectRetained(ctx)
	if err != nil {
		return retain("inspect", err)
	}
	refreshed, err := r.attempts.RecoverRetained(ctx, record.ID, attempt.RetainedEvidence{ObservedAt: time.Now(), Session: observed})
	if err != nil {
		return retain("authority", err)
	}
	record = refreshed
	out.Attempt = record
	scope.AdoptRetained()
	work := &auxiliary{runner: r, spec: Spec{TaskID: record.TaskID, TurnID: record.TurnID, Kind: record.Kind, Agent: record.Agent, Project: record.Project}, identity: record.WorkID, validate: validate, running: record, reserved: true}
	ask, askUser, observe := r.callbacks()
	options := lifecycle.Options{
		Attempts: r.attempts, Roster: r.roster, Sessions: r.sessions, Workspaces: r.workspaces, Actor: "agentexec",
		Spec: record.Spec, At: harness.Placement{Node: record.Node, Harness: record.Harness},
		Resume: true, Ask: ask, AskUser: askUser,
		Observe: func(p view.Progress) {
			EmitProgress(ctx, p)
			if observe != nil {
				observe(record, p)
			}
		},
		Validate: work.check, Finish: work.finish, Failed: work.failed,
		Settlement: auxiliarySettlement,
	}
	if record.State != attempt.Running && record.Result != nil && len(record.Result.Output) > 0 {
		// The command already ended and its output is on the record: the
		// completion is rebuilt from it, never from a second prompt.
		var saved auxiliaryOutput
		if json.Unmarshal(record.Result.Output, &saved) != nil || saved.WorkID != record.WorkID {
			return retain("output", errors.New("candidate output identity differs"))
		}
		var nativeErr error
		if saved.Error != "" {
			nativeErr = errors.New(saved.Error)
			if saved.Validation {
				work.invalid = nativeErr
			}
		}
		if nativeErr == nil && validate != nil {
			if err := validate(saved.Answer); err != nil {
				return retain("validation", err)
			}
		}
		options.Replay, options.Validate = &lifecycle.Outcome{Answer: saved.Answer, PromptSettled: true, Err: nativeErr}, nil
	}
	run, err := lifecycle.Reattach(ctx, options, record, session)
	out.Attempt, out.Usage, out.Answer = run.Record, run.Usage, run.Answer
	runErr, unresolved = work.settle(run, err)
	return out, runErr
}

func decodeAuxiliary(record attempt.Record, validate func(string) error) (out Result, err error) {
	out.Attempt, out.Usage = record, record.Usage
	var saved auxiliaryOutput
	if record.Result == nil || len(record.Result.Output) == 0 || json.Unmarshal(record.Result.Output, &saved) != nil || saved.WorkID != record.WorkID {
		return out, Blocked(record, "output", "读取原执行的完整结果", "已提交结果缺失或工作标识不一致。", "建议核对执行记录与已保存输出。", nil)
	}
	out.Answer = saved.Answer
	if saved.Validation {
		return out, &ValidationError{Cause: errors.New(saved.Error)}
	}
	if saved.Error != "" {
		return out, errors.New(saved.Error)
	}
	if record.State != attempt.Bound {
		return out, Blocked(record, "state", "核对结果的提交状态", "这份结果没有完整提交。", "建议核对原执行后继续。", nil)
	}
	if validate != nil {
		if err := validate(out.Answer); err != nil {
			return out, Blocked(record, "validation", "重新核对已提交输出", "当前校验条件与原执行的已提交结果不一致。", "建议确认原任务条件后继续。", err)
		}
	}
	return out, nil
}

func (r *Runner) cleanupAuxiliary(ctx context.Context, record attempt.Record) error {
	cleanup, stop := lifecycle.Cleanup(ctx)
	defer stop()
	if err := r.sessions.CloseSession(cleanup, harness.Placement{Node: record.Node, Harness: record.Harness}, record.Session); err != nil {
		return fmt.Errorf("close settled auxiliary session: %w", err)
	}
	r.roster.Release(cleanup, record.Node, record.ID)
	if err := r.workspaces.Discard(cleanup, record.Workspace); err != nil {
		return fmt.Errorf("discard auxiliary workspace: %w", err)
	}
	return nil
}
func clipAnswer(answer string) string {
	r := []rune(answer)
	if len(r) > 200 {
		r = r[:200]
	}
	return string(r)
}

func originalInput(record attempt.Record) (auxiliaryInput, error) {
	var output auxiliaryOutput
	if record.Result == nil || len(record.Result.Output) == 0 || json.Unmarshal(record.Result.Output, &output) != nil || output.Input == nil {
		return auxiliaryInput{}, errors.New("original auxiliary request unavailable")
	}
	input := *output.Input
	id, err := workID(input.Spec, input.Prompt)
	if err != nil || id != record.WorkID || output.WorkID != record.WorkID || input.Spec.TaskID != record.TaskID || input.Spec.TurnID != record.TurnID || input.Spec.Project != record.Project || input.Spec.Agent != record.Agent || input.Spec.Kind != record.Kind {
		return auxiliaryInput{}, errors.New("original auxiliary request identity differs")
	}
	return input, nil
}
func (r *Runner) OriginalSpec(ctx context.Context, id string) (Spec, error) {
	record, err := r.attempts.Get(ctx, id)
	if err != nil {
		return Spec{}, err
	}
	input, err := originalInput(record)
	return input.Spec, err
}
