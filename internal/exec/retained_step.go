package exec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/agentexec"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/task"
)

func retainedStepDetached(record attempt.Record, cause error) error {
	return &execution.RetainedObserverDetached{AttemptID: record.ID, NodeID: record.Node, SessionID: record.Session, Cause: errors.Join(harness.ErrStopUnconfirmed, cause)}
}
func stepPhase(state attempt.State) int {
	switch state {
	case attempt.Running:
		return 1
	case attempt.Snapshotted:
		return 2
	case attempt.Published:
		return 3
	case attempt.Durable:
		return 4
	case attempt.Verifying:
		return 5
	case attempt.BindReady:
		return 6
	case attempt.Bound:
		return 7
	default:
		return 0
	}
}

type retainedStepRunner interface {
	ResumeStep(context.Context, StepRequest, attempt.Record, func(nodewire.SessionState) error) (plan.StepResult, error)
}

func resumeRetainedStep(parent context.Context, p plan.Plan, step plan.Step, upstream []Result, deps Deps, record attempt.Record) (result plan.StepResult, runErr error) {
	hash, err := stepHash(p, step, upstream)
	if err != nil {
		return result, err
	}
	if record.Kind != attempt.KindStep || record.WorkID != hash || record.TaskID != p.TaskID || record.Project != p.ProjectID || record.TurnID != p.ID+"/"+step.ID || record.Execution == nil {
		return result, agentexec.Blocked(record, "identity", "核对原步骤的工作定义、计划与任务标识", "原步骤与当前计划定义不一致。", "建议核对原计划与已完成结果后继续。", ErrRecovery)
	}
	if deps.Executions == nil {
		return result, fmt.Errorf("%w: retained step execution registry unavailable", ErrRecovery)
	}
	scope, err := deps.Executions.BeginAccepted(parent, execution.Key{TaskID: record.TaskID, InstanceID: record.TurnID, AttemptID: record.ID}, record.Execution)
	if err != nil {
		return result, err
	}
	var unresolved error
	defer func() { scope.Finish(unresolved) }()
	ctx := scope.Context()
	blocked := func(code string, cause error) (plan.StepResult, error) {
		unresolved = retainedStepDetached(record, cause)
		return result, agentexec.Blocked(record, code, "连接原步骤的节点并核对已接受命令", "原步骤暂时不能安全接续。", "建议恢复原节点或存储，再检查同一次执行。", unresolved)
	}
	if record.State == attempt.Verifying && step.Verify != nil && step.Verify.Kind == plan.VerifyCommand && (record.SessionSettled == nil || !*record.SessionSettled) {
		return blocked("verify-command", errors.New("原 shell 验证的停止状态未知，不能用先前 Agent 回执替代"))
	}
	runner, ok := deps.Runner.(retainedStepRunner)
	if !ok {
		return blocked("runtime", errors.New("retained step runner unavailable"))
	}
	req := StepRequest{TaskID: p.TaskID, PlanID: p.ID, StepID: step.ID, Agent: record.Agent, Node: record.Node, Project: record.Project, Workspace: record.Workspace.Path, Goal: step.Goal}
	var saved plan.StepResult
	if record.State != attempt.Running {
		var output stepOutput
		if record.Result == nil || len(record.Result.Output) == 0 || json.Unmarshal(record.Result.Output, &output) != nil || output.PlanID != p.ID || output.StepID != step.ID || output.SpecHash != hash || output.Result.AttemptID != record.ID || !reflect.DeepEqual(output.Result.ExecutionToken, record.Execution) {
			return blocked("output", errors.New("original step output is unavailable or differs"))
		}
		saved = output.Result
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var stopBeat context.CancelFunc = func() {}
	result, runErr = runner.ResumeStep(runCtx, req, record, func(state nodewire.SessionState) error {
		refreshed, err := deps.Attempts.RecoverRetained(runCtx, record.ID, attempt.RetainedEvidence{ObservedAt: time.Now(), Session: state})
		if err != nil {
			return err
		}
		record = refreshed
		scope.AdoptRetained()
		beatCtx, stop := context.WithCancel(runCtx)
		stopBeat = stop
		lost := deps.Attempts.Heartbeat(beatCtx, record.ID)
		go func() {
			select {
			case <-lost:
				cancel()
			case <-beatCtx.Done():
			}
		}()
		return nil
	})
	defer stopBeat()
	if record.State != attempt.Running {
		result = saved
	}
	if acphost.PromptSettled(runErr) && errors.Is(execution.CheckExecution(runCtx), task.ErrExecutionStopped) {
		var cleanup context.CancelFunc
		runCtx, cleanup = context.WithTimeout(context.WithoutCancel(runCtx), 15*time.Second)
		defer cleanup()
		if runErr == nil {
			runErr = harness.ErrTurnCanceled
		}
	}
	if runCtx.Err() != nil || errors.Is(runErr, harness.ErrStopUnconfirmed) || !acphost.PromptSettled(runErr) {
		return blocked("observer", errors.Join(runErr, runCtx.Err()))
	}
	result.AttemptID, result.ExecutionToken = record.ID, record.Execution
	result.Agent, result.Node = record.Agent, record.Node
	result.PlanRevision = p.Rev
	result.StartedAt = record.StartedAt
	result.EndedAt = time.Now()
	if err := deps.Attempts.MarkSessionSettled(runCtx, record.ID, "exec-recovery"); err != nil {
		return blocked("marker", err)
	}
	fail := func(cause error) {
		failed, err := deps.Attempts.FailWith(runCtx, record.ID, "exec-recovery", cause.Error(), attemptUsage(result.Usage))
		if err != nil {
			unresolved = retainedStepDetached(record, err)
			return
		}
		if err := agentexec.SettleBudget(deps.Budget, failed, cause); err != nil {
			unresolved = agentexec.Blocked(failed, "accounting", "保存原步骤的用量与预算", "失败结果已保存，但预算结算尚未完成。", "建议恢复存储后核对同一次执行。", err)
			return
		}
		if err := cleanupFailedStep(runCtx, deps, failed); err != nil {
			unresolved = agentexec.Blocked(failed, "cleanup", "释放已结束步骤的原会话", "失败结果已保存，但原会话或工作区尚未释放。", "建议恢复原节点后重新检查。", err)
		}
	}
	if runErr != nil {
		result.Error = runErr.Error()
		fail(runErr)
		if unresolved != nil {
			return blocked("failure", unresolved)
		}
		return result, runErr
	}
	out, err := finishStep(runCtx, p, step, upstream, deps, record, req, result, stopBeat, fail, &unresolved)
	if err == nil {
		err = recordStepResult(deps, p.ID, step, out)
	}
	return out, err
}
func recordStepResult(deps Deps, id string, step plan.Step, result plan.StepResult) error {
	return record(deps, id, step, result, nil)
}

func retainedCandidate(record attempt.Record) bool {
	return record.Kind == attempt.KindStep && strings.HasPrefix(record.Session, "ns_") && !record.State.Terminal()
}

func cleanupFailedStep(ctx context.Context, deps Deps, record attempt.Record) error {
	if !strings.HasPrefix(record.Session, "ns_") {
		return nil
	}
	if !record.State.Terminal() || record.Unsettled || record.SessionSettled == nil || !*record.SessionSettled {
		return errors.New("step session has no durable settlement for cleanup")
	}
	closer, ok := deps.Runner.(interface {
		CloseRetainedStep(context.Context, attempt.Record) error
	})
	if !ok {
		return errors.New("retained step cleanup unavailable")
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	cleanup = execution.WithProbeKey(cleanup, execution.Key{TaskID: record.TaskID, InstanceID: record.TurnID, AttemptID: record.ID})
	if err := closer.CloseRetainedStep(cleanup, record); err != nil {
		return err
	}
	if deps.Roster != nil {
		deps.Roster.Release(cleanup, record.Node, record.ID)
	}
	if deps.Artifacts != nil {
		return deps.Artifacts.Discard(cleanup, record.Workspace)
	}
	return nil
}
