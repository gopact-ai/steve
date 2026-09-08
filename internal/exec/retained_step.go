package exec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/agentexec"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/lifecycle"
	"github.com/gopact-ai/steve/internal/plan"
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

// resumeRetainedStep joins a step whose node-owned session outlived its
// observer: the step is identified against the plan, the node's state
// checked against the record, and the attempt reattached from the prompt
// on — or, when the prompt had already ended, from the output on record.
func resumeRetainedStep(parent context.Context, p plan.Plan, step plan.Step, upstream []Result, deps Deps, record attempt.Record) (plan.StepResult, error) {
	hash, err := stepHash(p, step, upstream)
	if err != nil {
		return plan.StepResult{}, err
	}
	if record.Kind != attempt.KindStep || record.WorkID != hash || record.TaskID != p.TaskID || record.Project != p.ProjectID || record.TurnID != p.ID+"/"+step.ID || record.Execution == nil {
		return plan.StepResult{}, agentexec.Blocked(record, "identity", "核对原步骤的工作定义、计划与任务标识", "原步骤与当前计划定义不一致。", "建议核对原计划与已完成结果后继续。", ErrRecovery)
	}
	if deps.Executions == nil {
		return plan.StepResult{}, fmt.Errorf("%w: retained step execution registry unavailable", ErrRecovery)
	}
	scope, err := deps.Executions.BeginAccepted(parent, execution.Key{TaskID: record.TaskID, InstanceID: record.TurnID, AttemptID: record.ID}, record.Execution)
	if err != nil {
		return plan.StepResult{}, err
	}
	var unresolved error
	defer func() { scope.Finish(unresolved) }()
	ctx := scope.Context()
	blocked := func(code string, cause error) (plan.StepResult, error) {
		unresolved = retainedStepDetached(record, cause)
		return plan.StepResult{}, agentexec.Blocked(record, code, "连接原步骤的节点并核对已接受命令", "原步骤暂时不能安全接续。", "建议恢复原节点或存储，再检查同一次执行。", unresolved)
	}
	if record.State == attempt.Verifying && step.Verify != nil && step.Verify.Kind == plan.VerifyCommand && (record.SessionSettled == nil || !*record.SessionSettled) {
		return blocked("verify-command", errors.New("原 shell 验证的停止状态未知，不能用先前 Agent 回执替代"))
	}
	runner, ok := deps.Runner.(retainedStepRunner)
	if !ok {
		return blocked("runtime", errors.New("retained step runner unavailable"))
	}
	r := &stepRun{deps: deps, p: p, step: step, upstream: upstream, started: record.StartedAt, actor: "exec-recovery", joined: true, reserved: true,
		record: record, at: harness.Placement{Node: record.Node, Harness: record.Harness},
		req: StepRequest{TaskID: p.TaskID, PlanID: p.ID, StepID: step.ID, Agent: record.Agent, Node: record.Node, Project: record.Project, Workspace: record.Workspace.Path, Goal: step.Goal}}
	var replay *lifecycle.Outcome
	if record.State != attempt.Running {
		var output stepOutput
		if record.Result == nil || len(record.Result.Output) == 0 || json.Unmarshal(record.Result.Output, &output) != nil || output.PlanID != p.ID || output.StepID != step.ID || output.SpecHash != hash || output.Result.AttemptID != record.ID || !reflect.DeepEqual(output.Result.ExecutionToken, record.Execution) {
			return blocked("output", errors.New("original step output is unavailable or differs"))
		}
		// The command already ended and its output is on the record: the
		// completion is rebuilt from it, never from a second prompt.
		r.saved = &output.Result
		replay = &lifecycle.Outcome{Answer: output.Result.Answer, PromptSettled: true}
	}
	joined, err := runner.AttachStep(ctx, r.req, record)
	if err != nil {
		return blocked("observer", errors.Join(err, ctx.Err()))
	}
	refreshed, err := deps.Attempts.RecoverRetained(ctx, record.ID, attempt.RetainedEvidence{ObservedAt: time.Now(), Session: joined.State})
	if err != nil {
		return blocked("observer", errors.Join(err, ctx.Err()))
	}
	record = refreshed
	r.record = record
	scope.AdoptRetained()
	var result plan.StepResult
	result, err, unresolved = r.resume(ctx, joined, replay)
	if err == nil {
		err = recordStepResult(deps, p.ID, step, result)
	}
	return result, err
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
	closer, ok := deps.Runner.(retainedStepCloser)
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
