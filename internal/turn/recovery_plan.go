package turn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/gopact-ai/steve/internal/agentexec"
	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/exec"
	"github.com/gopact-ai/steve/internal/execution"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/plan"
	"github.com/gopact-ai/steve/internal/task"
)

type RetainedPlan struct {
	TaskID, Conversation, MessageID, PlanID, RunID string
	AttemptID, AgentID, NodeID, ProjectID          string
	Completed                                      bool
}

// SetPlanRecoveryOwner excludes adapter-owned exchanges from the background
// notifier. Their adapter preserves the original exchange, progress and asks.
func (c *Coordinator) SetPlanRecoveryOwner(owned func(task.Task) bool) {
	c.planRecoveryOwner = owned
}

func (c *Coordinator) retainedPlanRuns(ctx context.Context) ([]exec.RunRecord, error) {
	if reader, ok := c.supervisor.(retainedRunReader); ok {
		return reader.RetainedRuns(ctx)
	}
	return c.supervisor.OpenRuns(ctx)
}

// RetainedPlans also finds planning calls made before a plan/run existed. The
// task's persisted anchor is the original exchange; no command is reconstructed.
func (c *Coordinator) RetainedPlans(ctx context.Context) ([]RetainedPlan, error) {
	if c.supervisor == nil || c.plans == nil || c.tasks == nil {
		return nil, nil
	}
	runs, err := c.retainedPlanRuns(ctx)
	if err != nil {
		return nil, err
	}
	byTask := map[string]exec.RunRecord{}
	for _, run := range runs {
		if previous, ok := byTask[run.TaskID]; ok && previous.ID != run.ID {
			return nil, errors.New("multiple plan runs belong to one task")
		}
		byTask[run.TaskID] = run
	}
	var result []RetainedPlan
	for _, tracked := range c.tasks.List("") {
		if tracked.Origin != "plan" || tracked.Channel == "" || tracked.AnchorMessage == "" {
			continue
		}
		item := RetainedPlan{TaskID: tracked.ID, Conversation: tracked.Channel, MessageID: tracked.AnchorMessage, ProjectID: tracked.ProjectID}
		if stored, ok := c.plans.ForTask(tracked.ID); ok {
			item.PlanID = stored.ID
		}
		if run, ok := byTask[tracked.ID]; ok {
			if run.PlanID != item.PlanID || run.ProjectID != tracked.ProjectID {
				return nil, errors.New("retained plan and run identity differ")
			}
			item.RunID, item.Completed = run.RunID, run.Phase == exec.RunCompleted
		}
		if c.attempts != nil {
			records, err := c.attempts.ForTask(ctx, tracked.ID)
			if err != nil {
				return nil, err
			}
			var latest attempt.Record
			for _, record := range records {
				if record.Kind != attempt.KindPlan || record.State == attempt.Superseded || !strings.HasPrefix(record.Session, "ns_") && !agentexec.PendingOpen(record) {
					continue
				}
				if latest.ID == "" || record.StartedAt.After(latest.StartedAt) || record.StartedAt.Equal(latest.StartedAt) && record.ID > latest.ID {
					latest = record
				}
			}
			item.AttemptID, item.AgentID, item.NodeID = latest.ID, latest.Agent, latest.Node
		}
		if item.PlanID != "" || item.AttemptID != "" || tracked.PreparedPlan != nil {
			result = append(result, item)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].TaskID < result[j].TaskID })
	return result, nil
}

func planRecoveryError(err error) error {
	if err == nil {
		return nil
	}
	var blocked *RecoveryBlocked
	if errors.As(err, &blocked) {
		return blocked
	}
	var native *agentexec.RecoveryBlocked
	if errors.As(err, &native) {
		return &RecoveryBlocked{Question: native.Question, Cause: errors.Join(err, harness.ErrStopUnconfirmed)}
	}
	var nowhere exec.ErrNowhereToRun
	var noBudget exec.ErrNoBudget
	var exhausted exec.ErrExhausted
	if errors.As(err, &nowhere) || errors.As(err, &noBudget) || errors.As(err, &exhausted) {
		return retainedBlocked("plan-conditions", "检查可用机器、执行条件、剩余预算及允许的恢复方案", "计划目前无法继续推进。", err.Error(), "建议根据上述原因恢复所需机器或权限、调整预算，或提供其他可行方案；已有步骤和结果会保留。", err)
	}
	if errors.Is(err, harness.ErrStopUnconfirmed) || errors.Is(err, exec.ErrRecovery) || errors.Is(err, exec.ErrProjection) || errors.Is(err, exec.ErrCompletion) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return retainedBlocked("plan-execution", "读取计划检查点和原执行结果", "计划尚未完整完成。", err.Error(), "建议恢复节点与存储后重新检查，保留已完成步骤。", errors.Join(err, harness.ErrStopUnconfirmed))
	}
	return nil
}

// ResumeRetainedPlan resumes the admitted planning command or persisted run.
// It never invokes Handle or creates a second task for the original exchange.
func (c *Coordinator) ResumeRetainedPlan(parent context.Context, identity RetainedPlan, req Request) (Result, error) {
	c.requestMu.RLock()
	defer c.requestMu.RUnlock()
	var err error
	c, err = c.forChannel(req.Channel)
	if err != nil {
		return Result{}, err
	}
	c = c.localized(i18n.ContextLocale(parent))
	if req.Locale != "" {
		c = c.localized(i18n.FromLang(req.Locale))
	}
	if c.maintaining {
		return Result{}, retainedBlocked("maintenance", "检查协调服务", "协调服务正在交接或维护。", "暂时不能接续计划。", "建议等待交接完成后继续。", nil)
	}
	if c.tasks == nil || c.plans == nil || c.supervisor == nil {
		return Result{}, errors.New("retained plan recovery is not configured")
	}
	tracked, ok := c.tasks.Get(identity.TaskID)
	if !ok || tracked.Origin != "plan" || tracked.Channel != identity.Conversation || tracked.AnchorMessage != identity.MessageID || req.ConversationID != identity.Conversation || req.MessageID != identity.MessageID || tracked.Requester != "" && tracked.Requester != req.SenderOpenID || req.ExpectedProject != "" && req.ExpectedProject != tracked.ProjectID {
		return Result{}, retainedBlocked("plan-identity", "核对计划与原会话", "无法确认原计划的归属。", "任务、请求者、项目或会话标识不一致。", "建议核对原任务记录后继续。", nil)
	}
	ctx, cancel := context.WithTimeout(parent, planTimeout)
	defer cancel()
	ctx = agentexec.WithProgress(ctx, req.OnProgress)
	driver := "plan/" + tracked.ID
	if !c.beginTurn(req.ConversationID, driver, cancel) {
		return Result{}, retainedBlocked("plan-busy", "检查原计划的执行占用", "原计划已有一个驱动正在处理。", "需要等待当前观察者退出。", "建议稍后重新检查。", nil)
	}
	defer c.clearActive(req.ConversationID, driver)
	c.rememberMode(req)
	runs, err := c.retainedPlanRuns(ctx)
	if err != nil {
		return Result{}, err
	}
	for _, run := range runs {
		if run.TaskID != tracked.ID {
			continue
		}
		if run.ProjectID != tracked.ProjectID || identity.PlanID != "" && run.PlanID != identity.PlanID || identity.RunID != "" && run.RunID != identity.RunID {
			return Result{}, errors.New("retained plan run does not match original exchange")
		}
		if req.OnTurnReady != nil {
			req.OnTurnReady(tracked.ID, identity.AttemptID)
		}
		outcome, runErr := c.supervisor.Resume(ctx, run)
		return c.planExecutionResult(ctx, run.PlanID, outcome, runErr)
	}
	stored, exists := c.plans.ForTask(tracked.ID)
	if exists && (identity.PlanID != "" && identity.PlanID != stored.ID || stored.ProjectID != tracked.ProjectID) {
		return Result{}, errors.New("retained plan identity changed")
	}
	var token *task.ExecutionToken
	if exists {
		token = stored.Execution
	}
	if !exists && tracked.PreparedPlan != nil {
		token = &tracked.PreparedPlan.Execution
	}
	if token == nil && identity.AttemptID != "" && c.attempts != nil {
		record, err := c.attempts.Get(ctx, identity.AttemptID)
		if err != nil || record.TaskID != tracked.ID || record.Project != tracked.ProjectID || record.Kind != attempt.KindPlan {
			return Result{}, errors.Join(err, errors.New("retained planning identity differs"))
		}
		token = record.Execution
	}
	if token == nil || c.executions == nil {
		return Result{}, retainedBlocked("plan-authority", "检查原计划授权", "原计划缺少可核对的执行授权。", "不能使用后续任务的授权继续旧计划。", "建议核对原任务与执行记录。", nil)
	}
	scope, err := c.executions.BeginAccepted(ctx, execution.Key{TaskID: tracked.ID, InstanceID: driver, AttemptID: identity.AttemptID}, token)
	if err != nil {
		return Result{}, retainedBlocked("plan-authority", "核对原计划授权", "原计划暂时不能继续。", err.Error(), "建议确认任务是否暂停或取消。", err)
	}
	defer scope.Finish(nil)
	ctx = scope.Context()
	if req.OnTurnReady != nil {
		req.OnTurnReady(tracked.ID, identity.AttemptID)
	}
	if !exists {
		var proposed plan.Plan
		var err error
		if tracked.PreparedPlan != nil {
			proposed, err = preparedTaskPlan(tracked)
		} else {
			planner, ok := c.supervisor.(retainedPlanner)
			if !ok || identity.AttemptID == "" {
				return Result{}, errors.New("retained planning consumer unavailable")
			}
			proposed, err = planner.ResumePlanning(ctx, identity.AttemptID)
		}
		if err != nil {
			if blocked := planRecoveryError(err); blocked != nil {
				return Result{}, blocked
			}
			return Result{Title: c.text.T(i18n.CardPlan), Text: c.text.T(i18n.PlanFailed, err)}, nil
		}
		if proposed.TaskID != tracked.ID || proposed.ID != "" || proposed.Goal != tracked.Goal {
			return Result{}, fmt.Errorf("retained planning output does not belong to original task")
		}
		proposed.ProjectID, proposed.Execution = tracked.ProjectID, token
		stored, err = c.plans.Create(proposed)
		if err != nil {
			return Result{}, err
		}
	}
	outcome, runErr := c.supervisor.Execute(ctx, stored)
	return c.planExecutionResult(ctx, stored.ID, outcome, runErr)
}

func preparedTaskPlan(tracked task.Task) (plan.Plan, error) {
	var built plan.Plan
	if tracked.PreparedPlan == nil || tracked.PreparedPlan.Execution.TaskID != tracked.ID || json.Unmarshal(tracked.PreparedPlan.Snapshot, &built) != nil || built.ID != "" || built.TaskID != "" || built.Execution != nil || built.Goal != tracked.Goal || built.ProjectID != tracked.ProjectID || built.By != "rule" {
		return built, errors.New("prepared rule plan does not match original task")
	}
	if err := plan.Validate(built); err != nil {
		return built, err
	}
	built.TaskID = tracked.ID
	return built, nil
}
