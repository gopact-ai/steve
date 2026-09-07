package exec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/gopact-ai/steve/internal/agentexec"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/plan"
)

var ErrProjection = errors.New("completed execution projection could not be recorded")
var ErrRecovery = errors.New("execution recovery requires a valid committed result")

type stepOutput struct {
	PlanID   string          `json:"plan_id"`
	Revision int             `json:"revision"`
	StepID   string          `json:"step_id"`
	SpecHash string          `json:"spec_hash"`
	Attempts int             `json:"attempts"`
	Result   plan.StepResult `json:"result"`
}

func stepHash(p plan.Plan, step plan.Step, upstream []Result) (string, error) {
	// Mutable progress, budgets and the result are not the work's identity.
	step.State, step.Result, step.Attempts, step.Tried = "", nil, 0, nil
	inputs := make(map[string]string, len(upstream))
	for _, item := range upstream {
		inputs[item.StepID] = item.Result.Artifact
	}
	data, err := json.Marshal(struct {
		Project, Base string
		Step          plan.Step
		Inputs        map[string]string
	}{p.ProjectID, p.Base, step, inputs})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func encodeStepOutput(p plan.Plan, step plan.Step, upstream []Result, result plan.StepResult) (json.RawMessage, error) {
	hash, err := stepHash(p, step, upstream)
	if err != nil {
		return nil, err
	}
	return json.Marshal(stepOutput{PlanID: p.ID, Revision: p.Rev, StepID: step.ID, SpecHash: hash, Attempts: step.Attempts, Result: result})
}

func restoreStep(ctx context.Context, p plan.Plan, step *plan.Step, upstream []Result, deps Deps) (plan.StepResult, bool, error) {
	if deps.Attempts == nil {
		return plan.StepResult{}, false, nil
	}
	var r attempt.Record
	var err error
	var found bool
	fromCache := step.State == plan.StepDone && step.Result != nil
	if fromCache {
		if step.Result.AttemptID == "" {
			return plan.StepResult{}, false, fmt.Errorf("%w: step %s has no attempt identity", ErrRecovery, step.ID)
		}
		r, err = deps.Attempts.Get(ctx, step.Result.AttemptID)
		found = err == nil
	} else {
		r, found, err = deps.Attempts.LatestForTurn(ctx, p.ID+"/"+step.ID)
	}
	if err != nil {
		return plan.StepResult{}, false, fmt.Errorf("%w: %w", ErrRecovery, err)
	}
	if !found || r.State != attempt.Bound {
		if found {
			hash, err := stepHash(p, *step, upstream)
			if err != nil {
				return plan.StepResult{}, false, err
			}
			if r.WorkID == "" || r.WorkID != hash {
				return plan.StepResult{}, false, fmt.Errorf("%w: unfinished attempt %s has different or missing work identity", ErrRecovery, r.ID)
			}
			all, err := deps.Attempts.ForTask(ctx, p.TaskID)
			if err != nil {
				return plan.StepResult{}, false, fmt.Errorf("%w: %w", ErrRecovery, err)
			}
			count := 0
			for _, prior := range all {
				if prior.TurnID == p.ID+"/"+step.ID && prior.WorkID == hash {
					count++
				}
			}
			step.Attempts = max(step.Attempts, count)
		}
		if found && !fromCache && retainedCandidate(r) {
			return plan.StepResult{}, false, nil
		}
		if found && r.Result != nil && len(r.Result.Output) > 0 {
			return plan.StepResult{}, false, fmt.Errorf("%w: attempt %s has an uncommitted candidate result", ErrRecovery, r.ID)
		}
		if found && (r.Unsettled || (r.State != attempt.Failed && r.State != attempt.Expired && r.State != attempt.Superseded)) {
			return plan.StepResult{}, false, fmt.Errorf("%w: attempt %s is %s", ErrRecovery, r.ID, r.State)
		}
		if fromCache {
			return plan.StepResult{}, false, fmt.Errorf("%w: step %s is not bound", ErrRecovery, step.ID)
		}
		if found && step.Attempts >= MaxRecoveries+1 {
			return plan.StepResult{}, false, fmt.Errorf("%w: step %s exhausted its retries", ErrRecovery, step.ID)
		}
		if found && r.State.Terminal() && !r.Unsettled {
			if err := agentexec.SettleBudget(deps.Budget, r, nil); err != nil {
				return plan.StepResult{}, false, agentexec.Blocked(r, "accounting", "核对原步骤的用量与预算", "原执行已结束，但预算结算尚未完成。", "建议恢复存储后重新检查。", err)
			}
			if err := cleanupFailedStep(ctx, deps, r); err != nil {
				return plan.StepResult{}, false, agentexec.Blocked(r, "cleanup", "释放已结束步骤的原会话", "失败结果已保存，但原会话或工作区尚未释放。", "建议恢复原节点后重新检查。", err)
			}
		}
		return plan.StepResult{}, false, nil
	}
	var output stepOutput
	if r.Result == nil || len(r.Result.Output) == 0 {
		return plan.StepResult{}, false, fmt.Errorf("%w: bound attempt %s has no output", ErrRecovery, r.ID)
	}
	if err := json.Unmarshal(r.Result.Output, &output); err != nil {
		return plan.StepResult{}, false, fmt.Errorf("%w: attempt %s: %w", ErrRecovery, r.ID, err)
	}
	hash, err := stepHash(p, *step, upstream)
	if err != nil {
		return plan.StepResult{}, false, err
	}
	if output.PlanID != p.ID || output.StepID != step.ID || output.Revision > p.Rev || output.Result.AttemptID != r.ID || output.Result.Artifact != r.Result.Artifact || !reflect.DeepEqual(output.Result.ExecutionToken, r.Execution) {
		return plan.StepResult{}, false, fmt.Errorf("%w: identity mismatch on %s", ErrRecovery, r.ID)
	}
	if output.SpecHash != hash {
		return plan.StepResult{}, false, fmt.Errorf("%w: completed step %s definition changed", ErrRecovery, step.ID)
	}
	if deps.Executions != nil && r.Execution != nil {
		scope, err := deps.Executions.BeginAccepted(ctx, execution.Key{TaskID: r.TaskID, AttemptID: r.ID, InstanceID: "restore/" + r.ID}, r.Execution)
		if err != nil {
			return plan.StepResult{}, false, fmt.Errorf("%w: %w", ErrRecovery, err)
		}
		scope.Finish(nil)
	}
	if err := agentexec.SettleBudget(deps.Budget, r, nil); err != nil {
		return plan.StepResult{}, false, agentexec.Blocked(r, "accounting", "核对已提交步骤的用量与预算", "步骤结果已提交，但预算结算尚未完成。", "建议恢复存储后核对同一次执行。", err)
	}
	step.Attempts = max(step.Attempts, output.Attempts)
	return output.Result, true, nil
}

// restorePlan validates every cached/committed result before the workflow
// may reuse checkpoint outputs that skip node callbacks.
func restorePlan(ctx context.Context, p plan.Plan, deps Deps) (plan.Plan, error) {
	p.Steps = append([]plan.Step(nil), p.Steps...)
	byID := map[string]plan.Step{}
	for _, step := range p.Steps {
		byID[step.ID] = step
	}
	results := map[string]Result{}
	visited := map[string]bool{}
	var visit func(string) error
	visit = func(id string) error {
		if visited[id] {
			return nil
		}
		visited[id] = true
		step, ok := byID[id]
		if !ok {
			return fmt.Errorf("%w: dependency %s missing", ErrRecovery, id)
		}
		var upstream []Result
		for _, dep := range dependencies(step) {
			if err := visit(dep); err != nil {
				return err
			}
			if result, ok := results[dep]; ok {
				upstream = append(upstream, result)
			}
		}
		result, found, err := restoreStep(ctx, p, &step, upstream, deps)
		if err != nil {
			return err
		}
		if found {
			step.State, step.Result = plan.StepDone, &result
			results[id] = Result{StepID: id, Result: result}
			if err := record(deps, p.ID, step, result, nil); err != nil {
				return err
			}
		}
		byID[id] = step
		return nil
	}
	for _, step := range p.Steps {
		if err := visit(step.ID); err != nil {
			return p, err
		}
	}
	for i, step := range p.Steps {
		p.Steps[i] = byID[step.ID]
	}
	return p, nil
}
