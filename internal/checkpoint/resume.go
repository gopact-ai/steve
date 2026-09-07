package checkpoint

import (
	"fmt"
	"slices"
	"strings"
)

type ResumeAction string

const (
	ResumeReattach     ResumeAction = "reattach"
	ResumeStartAttempt ResumeAction = "start-attempt"
	ResumeAskUser      ResumeAction = "ask-user"
)

type LiveAttempt struct {
	Source Source
	// Attached requires a successful authenticated process/session reattach
	// handshake. A heartbeat or an open socket alone cannot set it.
	Attached       bool
	AuthorityValid bool
	Cursors        Cursors
}

type IsolationKind string

const (
	IsolationProcessStopped  IsolationKind = "process-stopped"
	IsolationResourcesFenced IsolationKind = "resources-fenced"
)

// IsolationEvidence describes positively verified writer isolation. Stopped
// means actual process exit/physical fencing, not lease or heartbeat expiry.
// Resource fencing is sufficient only when every write and external effect
// passes an enforcing authority. Native agents with direct local filesystem
// or external-service access ordinarily require process-stop evidence.
type IsolationEvidence struct {
	AttemptID        string
	ExecutionEpoch   uint64
	Kind             IsolationKind
	Reference        string
	AllEffectsFenced bool
}

type ActionOutcome string

const (
	ActionApplied    ActionOutcome = "applied"
	ActionNotApplied ActionOutcome = "not-applied"
	// ActionRetryAuthorized records explicit consent to retry a named uncertain
	// operation in one plan; it never claims the operation did not happen.
	ActionRetryAuthorized ActionOutcome = "retry-authorized"
)

type ActionResolution struct {
	ActionID string        `json:"action_id"`
	Outcome  ActionOutcome `json:"outcome"`
	Evidence string        `json:"evidence"`
}

// ActionReconciliation must inspect the authoritative operation journal after
// isolation. A checkpoint cannot describe side effects issued after it was
// saved, so replay is blocked without this additional inspection.
type ActionReconciliation struct {
	AttemptID          string
	ExecutionEpoch     uint64
	IsolationReference string
	Checked            bool
	AdditionalUnknown  []ExternalAction
	Results            []ActionResolution
	RetryAuthorization *RetryAuthorization
}

type RetryAuthorization struct {
	PlanID       string `json:"plan_id"`
	TaskID       string `json:"task_id"`
	AttemptID    string `json:"attempt_id"`
	Actor        string `json:"actor"`
	DecisionID   string `json:"decision_id"`
	TargetNodeID string `json:"target_node_id"`
}

type CapabilityIssue struct {
	Kind           string           `json:"kind"`
	Name           string           `json:"name"`
	Attempted      []string         `json:"attempted,omitempty"`
	Problem        string           `json:"problem"`
	Reason         string           `json:"reason"`
	Recommendation string           `json:"recommendation"`
	Options        []RecoveryOption `json:"options,omitempty"`
}

type ResumeTarget struct {
	NodeID         string
	ExecutionEpoch uint64
	TaskEpoch      uint64
	// Authorized and AdmissionChecked come from fresh task/placement checks;
	// the attempt creator must repeat them atomically with execution admission.
	Authorized       bool
	AdmissionChecked bool
	Missing          []CapabilityIssue
}

type ResumeRequest struct {
	Source         Source
	Live           *LiveAttempt
	Checkpoint     *VerifiedCheckpoint
	Isolation      IsolationEvidence
	Reconciliation ActionReconciliation
	Target         ResumeTarget
	Attempted      []string
}

type RecoveryOption struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	Recommended bool   `json:"recommended,omitempty"`
}

// RecoveryQuestion is durable Ask User content, not a terminal task failure.
// It records actual attempted checks, their obstacle and a suggested decision.
// Responses must be persisted on the original task/session and followed by a
// fresh ResumePlan; accepting an option is not itself execution authority.
type RecoveryQuestion struct {
	Code           string           `json:"code"`
	TaskID         string           `json:"task_id"`
	SessionID      string           `json:"session_id"`
	AttemptID      string           `json:"attempt_id"`
	Title          string           `json:"title"`
	Attempted      []string         `json:"attempted"`
	Problem        string           `json:"problem"`
	Reason         string           `json:"reason"`
	Recommendation string           `json:"recommendation"`
	Options        []RecoveryOption `json:"options"`
	AllowFreeText  bool             `json:"allow_free_text"`
}

// Message renders the same structured explanation for existing Ask User
// consumers that accept text and choices rather than separate diagnostic fields.
func (q RecoveryQuestion) Message() string {
	var parts []string
	if len(q.Attempted) > 0 {
		parts = append(parts, "已尝试："+strings.Join(q.Attempted, "；"))
	}
	parts = append(parts, q.Problem, q.Reason, q.Recommendation)
	return strings.Join(parts, "\n\n")
}

type ResumeDecision struct {
	Action            ResumeAction `json:"action"`
	TaskID            string       `json:"task_id"`
	SessionID         string       `json:"session_id"`
	TurnID            string       `json:"turn_id"`
	PreviousAttemptID string       `json:"previous_attempt_id"`
	// AttemptID is retained for reattach and empty for start-attempt. The
	// execution service mints a fresh ID only after transactional admission.
	AttemptID         string             `json:"attempt_id,omitempty"`
	NodeID            string             `json:"node_id,omitempty"`
	ExecutionEpoch    uint64             `json:"execution_epoch,omitempty"`
	TaskEpoch         uint64             `json:"task_epoch"`
	CheckpointID      string             `json:"checkpoint_id,omitempty"`
	Cursors           Cursors            `json:"cursors"`
	ReconciledActions []ActionResolution `json:"reconciled_actions,omitempty"`
	Question          *RecoveryQuestion  `json:"question,omitempty"`
}

// ResumePlan is a conservative, side-effect-free recovery decision. It cannot
// stop a process, revoke a writer or grant a new attempt's leases. Those facts
// must come from their enforcing services and are checked again on execution.
func ResumePlan(req ResumeRequest) ResumeDecision {
	source := req.Source
	decision := ResumeDecision{TaskID: source.TaskID, SessionID: source.SessionID, TurnID: source.TurnID, PreviousAttemptID: source.AttemptID, TaskEpoch: source.TaskEpoch}
	attempted := slices.Clone(req.Attempted)
	ask := func(code, problem, reason, recommendation string, options ...RecoveryOption) ResumeDecision {
		if len(options) == 0 {
			options = []RecoveryOption{{ID: "retry-checks", Label: "重新检查", Recommended: true}, {ID: "wait", Label: "暂时等待"}}
		}
		decision.Action = ResumeAskUser
		decision.Question = &RecoveryQuestion{Code: code, TaskID: source.TaskID, SessionID: source.SessionID, AttemptID: source.AttemptID, Title: "继续任务需要你的决定", Attempted: slices.Clone(attempted), Problem: problem, Reason: reason, Recommendation: recommendation, Options: slices.Clone(options), AllowFreeText: true}
		return decision
	}
	if err := validateSource(source); err != nil {
		attempted = append(attempted, "检查任务、会话与原执行的关联记录")
		return ask("source-incomplete", "原执行的身份记录不完整。", "无法安全判断应接续哪个执行。", "建议先恢复原节点的执行记录，再继续这个任务。")
	}
	if req.Live != nil {
		attempted = append(attempted, "检查原执行的会话接续结果和当前执行授权")
		if req.Live.Source == source && req.Live.Attached && req.Live.AuthorityValid {
			decision.Action, decision.AttemptID, decision.NodeID = ResumeReattach, source.AttemptID, source.NodeID
			decision.ExecutionEpoch, decision.Cursors = source.ExecutionEpoch, req.Live.Cursors
			return decision
		}
	}
	attempted = append(attempted, "检查完整检查点和目标节点上的内容校验结果")
	if req.Checkpoint == nil || req.Checkpoint.manifest.ID == "" {
		return ask("checkpoint-unavailable", "暂时没有可用的完整恢复检查点。", "未完成传输的材料或工作区不能用于恢复；原执行的进度仍保留在已有记录中。", "建议让原节点恢复连接，或提供其他已完整保存的检查点。", RecoveryOption{ID: "wait-source", Label: "等待原机器恢复", Recommended: true}, RecoveryOption{ID: "locate-checkpoint", Label: "查找其他检查点"})
	}
	m := req.Checkpoint.manifest
	if m.Snapshot.Source != source {
		return ask("checkpoint-mismatch", "检查点与这个任务的原执行不匹配。", "不同任务、会话或执行代际的数据不能相互替代。", "建议重新定位这个执行已提交的检查点。")
	}
	if req.Checkpoint.nodeID != req.Target.NodeID || !validID(req.Target.NodeID) {
		return ask("checkpoint-not-local", "目标节点上尚未验证完整恢复内容。", "只有文件摘要不足以启动新执行，需要获取并校验实际内容。", "建议先将检查点同步到目标节点，再继续这个任务。")
	}
	decision = CheckReplacement(ReplacementRequest{PlanID: m.ID, Source: source, Isolation: req.Isolation, Reconciliation: req.Reconciliation, Target: req.Target, UnknownActions: m.Snapshot.UnknownActions, Attempted: attempted})
	if decision.Action == ResumeStartAttempt {
		decision.CheckpointID, decision.Cursors = m.ID, m.Snapshot.Cursors
	}
	return decision
}

// ReplacementRequest carries execution-safety facts after the caller's content
// consumer verified a complete recovery source. CheckReplacement does not verify
// bytes or grant execution; portable blob checkpoints and verified Git bundles
// use it in addition to their own content and placement checks.
type ReplacementRequest struct {
	PlanID         string
	Source         Source
	Isolation      IsolationEvidence
	Reconciliation ActionReconciliation
	Target         ResumeTarget
	UnknownActions []ExternalAction
	Attempted      []string
}

// CheckReplacement applies the same isolation, reconciliation and authorization
// boundary regardless of the verified recovery content's storage format.
func CheckReplacement(req ReplacementRequest) ResumeDecision {
	source := req.Source
	decision := ResumeDecision{TaskID: source.TaskID, SessionID: source.SessionID, TurnID: source.TurnID, PreviousAttemptID: source.AttemptID, TaskEpoch: source.TaskEpoch}
	attempted := slices.Clone(req.Attempted)
	ask := func(code, problem, reason, recommendation string, options ...RecoveryOption) ResumeDecision {
		if len(options) == 0 {
			options = []RecoveryOption{{ID: "retry-checks", Label: "重新检查", Recommended: true}, {ID: "wait", Label: "暂时等待"}}
		}
		decision.Action = ResumeAskUser
		decision.Question = &RecoveryQuestion{Code: code, TaskID: source.TaskID, SessionID: source.SessionID, AttemptID: source.AttemptID, Title: "继续任务需要你的决定", Attempted: slices.Clone(attempted), Problem: problem, Reason: reason, Recommendation: recommendation, Options: slices.Clone(options), AllowFreeText: true}
		return decision
	}
	if err := validateSource(source); err != nil {
		return ask("source-incomplete", "原执行的身份记录不完整。", "无法核对这个执行的权限和来源。", "建议先核对原执行记录。")
	}
	attempted = append(attempted, "核对原执行的停止或写入隔离证据")
	fence := req.Isolation
	isolated := fence.AttemptID == source.AttemptID && fence.ExecutionEpoch == source.ExecutionEpoch && strings.TrimSpace(fence.Reference) != "" && (fence.Kind == IsolationProcessStopped || (fence.Kind == IsolationResourcesFenced && fence.AllEffectsFenced))
	if !isolated {
		return ask("writer-not-isolated", "还无法确认原执行已经停止写入。", "连接中断、心跳超时或协调节点切换都不能证明原进程已停止；直接重跑可能产生重复操作。", "建议先恢复原机器连接以接续执行，或由执行服务验证原进程停止后再恢复。", RecoveryOption{ID: "reconnect-source", Label: "重连原机器", Recommended: true}, RecoveryOption{ID: "verify-stop", Label: "检查原执行是否停止"}, RecoveryOption{ID: "wait", Label: "暂时等待"})
	}
	attempted = append(attempted, "核对检查点之后的外部操作及其结果")
	reconciliation := req.Reconciliation
	if !reconciliation.Checked || reconciliation.AttemptID != source.AttemptID || reconciliation.ExecutionEpoch != source.ExecutionEpoch || reconciliation.IsolationReference != fence.Reference {
		return ask("actions-not-reconciled", "尚未完成原执行的外部操作对账。", "检查点之后可能已经发出操作；需要在原执行被隔离后确认结果，才能决定哪些工作可以继续。", "建议检查原执行的操作记录，再从检查点恢复。", RecoveryOption{ID: "reconcile-actions", Label: "检查操作结果", Recommended: true}, RecoveryOption{ID: "wait", Label: "暂时等待"})
	}
	unknown := slices.Concat(req.UnknownActions, reconciliation.AdditionalUnknown)
	resolved := map[string]ActionResolution{}
	for _, result := range reconciliation.Results {
		if previous, ok := resolved[result.ActionID]; ok && previous != result {
			return ask("action-result-conflict", "同一个外部操作存在不一致的结果记录。", "无法确定这个操作是否已经成功，直接继续可能重复执行。", "建议先核对冲突的操作记录。")
		}
		authorizedRetry := false
		if authorization := reconciliation.RetryAuthorization; authorization != nil {
			authorizedRetry = result.Outcome == ActionRetryAuthorized && authorization.TaskID == source.TaskID && authorization.AttemptID == source.AttemptID && authorization.PlanID != "" && authorization.PlanID == req.PlanID && authorization.TargetNodeID == req.Target.NodeID && authorization.Actor != "" && authorization.DecisionID == "confirm-stopped-and-retry:"+authorization.PlanID && result.Evidence == authorization.DecisionID
		}
		if validID(result.ActionID) && (result.Outcome == ActionApplied || result.Outcome == ActionNotApplied || authorizedRetry) && strings.TrimSpace(result.Evidence) != "" {
			resolved[result.ActionID] = result
		}
	}
	for _, action := range unknown {
		if _, ok := resolved[action.ID]; !ok {
			return ask("action-result-unknown", "尚无法确认外部操作的结果："+action.Description, "这个操作可能已经执行成功，不能在新节点上直接重放。", "建议根据操作记录查询实际结果，确认后继续后续工作。", RecoveryOption{ID: "reconcile-actions", Label: "核对这个操作", Recommended: true}, RecoveryOption{ID: "wait", Label: "暂时等待"})
		}
	}
	attempted = append(attempted, "检查目标节点的任务授权、执行代际和所需能力")
	if !req.Target.Authorized || req.Target.TaskEpoch != source.TaskEpoch || req.Target.ExecutionEpoch <= source.ExecutionEpoch {
		return ask("execution-not-authorized", "还没有有效的新执行授权。", "新执行必须使用递增的写入代际，并继续遵守用户当前的任务授权。", "建议重新检查任务状态与执行授权；若任务已暂停，请先决定是否恢复。")
	}
	if !req.Target.AdmissionChecked {
		return ask("capabilities-unchecked", "目标节点尚未完成执行能力检查。", "目标节点的 Agent、模型、工具、网络、工作区及登录状态可能与原节点不同。", "建议完成目标节点的能力检查后再继续任务。")
	}
	if len(req.Target.Missing) > 0 {
		issue := req.Target.Missing[0]
		attempted = append(attempted, issue.Attempted...)
		problem, reason, recommendation := issue.Problem, issue.Reason, issue.Recommendation
		if strings.TrimSpace(problem) == "" {
			problem = fmt.Sprintf("目标节点暂时无法满足 %s。", issue.Name)
		}
		if strings.TrimSpace(reason) == "" {
			reason = "重新检查后仍缺少执行所需条件，已有进度已保存在同一个任务中。"
		}
		if strings.TrimSpace(recommendation) == "" {
			recommendation = "建议补齐这个条件，或选择具备所需条件的节点。"
		}
		if len(issue.Options) == 0 {
			issue.Options = []RecoveryOption{{ID: "choose-node", Label: "选择其他机器", Recommended: true}, {ID: "retry-checks", Label: "补齐条件后重试"}, {ID: "wait", Label: "暂时等待"}}
		}
		return ask("capability-missing", problem, reason, recommendation, issue.Options...)
	}
	decision.Action, decision.NodeID = ResumeStartAttempt, req.Target.NodeID
	decision.ExecutionEpoch = req.Target.ExecutionEpoch
	decision.ReconciledActions = make([]ActionResolution, 0, len(resolved))
	for _, result := range resolved {
		decision.ReconciledActions = append(decision.ReconciledActions, result)
	}
	slices.SortFunc(decision.ReconciledActions, func(a, b ActionResolution) int { return strings.Compare(a.ActionID, b.ActionID) })
	return decision
}
