package checkpoint

import (
	"slices"
	"strings"

	"github.com/gopact-ai/steve/internal/i18n"
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
func (q RecoveryQuestion) Message(text i18n.Catalog) string {
	var parts []string
	if len(q.Attempted) > 0 {
		parts = append(parts, text.T(i18n.RecoveryTried, strings.Join(q.Attempted, text.T(i18n.ResumeAttemptedSeparator))))
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
func ResumePlan(text i18n.Catalog, req ResumeRequest) ResumeDecision {
	source := req.Source
	decision := ResumeDecision{TaskID: source.TaskID, SessionID: source.SessionID, TurnID: source.TurnID, PreviousAttemptID: source.AttemptID, TaskEpoch: source.TaskEpoch}
	attempted := slices.Clone(req.Attempted)
	ask := func(code, problem, reason, recommendation string, options ...RecoveryOption) ResumeDecision {
		if len(options) == 0 {
			options = []RecoveryOption{{ID: "retry-checks", Label: text.T(i18n.ResumeOptionRecheck), Recommended: true}, {ID: "wait", Label: text.T(i18n.ResumeOptionWait)}}
		}
		decision.Action = ResumeAskUser
		decision.Question = &RecoveryQuestion{Code: code, TaskID: source.TaskID, SessionID: source.SessionID, AttemptID: source.AttemptID, Title: text.T(i18n.ResumeTitle), Attempted: slices.Clone(attempted), Problem: problem, Reason: reason, Recommendation: recommendation, Options: slices.Clone(options), AllowFreeText: true}
		return decision
	}
	if err := validateSource(source); err != nil {
		attempted = append(attempted, text.T(i18n.ResumeTriedSourceLinks))
		return ask("source-incomplete", text.T(i18n.ResumeProblemSourceIncomplete), text.T(i18n.ResumeReasonSourceAmbiguous), text.T(i18n.ResumeAdviceRestoreSourceRecord))
	}
	if req.Live != nil {
		attempted = append(attempted, text.T(i18n.ResumeTriedLiveAttempt))
		if req.Live.Source == source && req.Live.Attached && req.Live.AuthorityValid {
			decision.Action, decision.AttemptID, decision.NodeID = ResumeReattach, source.AttemptID, source.NodeID
			decision.ExecutionEpoch, decision.Cursors = source.ExecutionEpoch, req.Live.Cursors
			return decision
		}
	}
	attempted = append(attempted, text.T(i18n.ResumeTriedCheckpoint))
	if req.Checkpoint == nil || req.Checkpoint.manifest.ID == "" {
		return ask("checkpoint-unavailable", text.T(i18n.ResumeProblemNoCheckpoint), text.T(i18n.ResumeReasonPartialCopy), text.T(i18n.ResumeAdviceReconnectOrCheckpoint), RecoveryOption{ID: "wait-source", Label: text.T(i18n.ResumeOptionWaitSource), Recommended: true}, RecoveryOption{ID: "locate-checkpoint", Label: text.T(i18n.ResumeOptionLocateCheckpoint)})
	}
	m := req.Checkpoint.manifest
	if m.Snapshot.Source != source {
		return ask("checkpoint-mismatch", text.T(i18n.ResumeProblemCheckpointMismatch), text.T(i18n.ResumeReasonNotInterchangeable), text.T(i18n.ResumeAdviceLocateCheckpoint))
	}
	if req.Checkpoint.nodeID != req.Target.NodeID || !validID(req.Target.NodeID) {
		return ask("checkpoint-not-local", text.T(i18n.ResumeProblemNotLocal), text.T(i18n.ResumeReasonDigestOnly), text.T(i18n.ResumeAdviceSyncCheckpoint))
	}
	decision = CheckReplacement(text, ReplacementRequest{PlanID: m.ID, Source: source, Isolation: req.Isolation, Reconciliation: req.Reconciliation, Target: req.Target, UnknownActions: m.Snapshot.UnknownActions, Attempted: attempted})
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
func CheckReplacement(text i18n.Catalog, req ReplacementRequest) ResumeDecision {
	source := req.Source
	decision := ResumeDecision{TaskID: source.TaskID, SessionID: source.SessionID, TurnID: source.TurnID, PreviousAttemptID: source.AttemptID, TaskEpoch: source.TaskEpoch}
	attempted := slices.Clone(req.Attempted)
	ask := func(code, problem, reason, recommendation string, options ...RecoveryOption) ResumeDecision {
		if len(options) == 0 {
			options = []RecoveryOption{{ID: "retry-checks", Label: text.T(i18n.ResumeOptionRecheck), Recommended: true}, {ID: "wait", Label: text.T(i18n.ResumeOptionWait)}}
		}
		decision.Action = ResumeAskUser
		decision.Question = &RecoveryQuestion{Code: code, TaskID: source.TaskID, SessionID: source.SessionID, AttemptID: source.AttemptID, Title: text.T(i18n.ResumeTitle), Attempted: slices.Clone(attempted), Problem: problem, Reason: reason, Recommendation: recommendation, Options: slices.Clone(options), AllowFreeText: true}
		return decision
	}
	if err := validateSource(source); err != nil {
		return ask("source-incomplete", text.T(i18n.ResumeProblemSourceIncomplete), text.T(i18n.ResumeReasonSourceUnverifiable), text.T(i18n.ResumeAdviceCheckSourceRecord))
	}
	attempted = append(attempted, text.T(i18n.ResumeTriedIsolation))
	fence := req.Isolation
	isolated := fence.AttemptID == source.AttemptID && fence.ExecutionEpoch == source.ExecutionEpoch && strings.TrimSpace(fence.Reference) != "" && (fence.Kind == IsolationProcessStopped || (fence.Kind == IsolationResourcesFenced && fence.AllEffectsFenced))
	if !isolated {
		return ask("writer-not-isolated", text.T(i18n.ResumeProblemWriterLive), text.T(i18n.ResumeReasonNoStopProof), text.T(i18n.ResumeAdviceReconnectOrVerifyStop), RecoveryOption{ID: "reconnect-source", Label: text.T(i18n.ResumeOptionReconnectSource), Recommended: true}, RecoveryOption{ID: "verify-stop", Label: text.T(i18n.ResumeOptionVerifyStop)}, RecoveryOption{ID: "wait", Label: text.T(i18n.ResumeOptionWait)})
	}
	attempted = append(attempted, text.T(i18n.ResumeTriedActions))
	reconciliation := req.Reconciliation
	if !reconciliation.Checked || reconciliation.AttemptID != source.AttemptID || reconciliation.ExecutionEpoch != source.ExecutionEpoch || reconciliation.IsolationReference != fence.Reference {
		return ask("actions-not-reconciled", text.T(i18n.ResumeProblemActionsUnreconciled), text.T(i18n.ResumeReasonActionsAfterCheckpoint), text.T(i18n.ResumeAdviceCheckActions), RecoveryOption{ID: "reconcile-actions", Label: text.T(i18n.ResumeOptionReconcileActions), Recommended: true}, RecoveryOption{ID: "wait", Label: text.T(i18n.ResumeOptionWait)})
	}
	unknown := slices.Concat(req.UnknownActions, reconciliation.AdditionalUnknown)
	resolved := map[string]ActionResolution{}
	for _, result := range reconciliation.Results {
		if previous, ok := resolved[result.ActionID]; ok && previous != result {
			return ask("action-result-conflict", text.T(i18n.ResumeProblemActionConflict), text.T(i18n.ResumeReasonActionMayHaveRun), text.T(i18n.ResumeAdviceCheckConflict))
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
			return ask("action-result-unknown", text.T(i18n.ResumeProblemActionUnknown, action.Description), text.T(i18n.ResumeReasonNoReplay), text.T(i18n.ResumeAdviceQueryActual), RecoveryOption{ID: "reconcile-actions", Label: text.T(i18n.ResumeOptionReconcileAction), Recommended: true}, RecoveryOption{ID: "wait", Label: text.T(i18n.ResumeOptionWait)})
		}
	}
	attempted = append(attempted, text.T(i18n.ResumeTriedTarget))
	if !req.Target.Authorized || req.Target.TaskEpoch != source.TaskEpoch || req.Target.ExecutionEpoch <= source.ExecutionEpoch {
		return ask("execution-not-authorized", text.T(i18n.ResumeProblemUnauthorized), text.T(i18n.ResumeReasonEpochAndGrant), text.T(i18n.ResumeAdviceRecheckAuthorization))
	}
	if !req.Target.AdmissionChecked {
		return ask("capabilities-unchecked", text.T(i18n.ResumeProblemCapabilitiesUnchecked), text.T(i18n.ResumeReasonTargetDiffers), text.T(i18n.ResumeAdviceCheckCapabilities))
	}
	if len(req.Target.Missing) > 0 {
		issue := req.Target.Missing[0]
		attempted = append(attempted, issue.Attempted...)
		problem, reason, recommendation := issue.Problem, issue.Reason, issue.Recommendation
		if strings.TrimSpace(problem) == "" {
			problem = text.T(i18n.ResumeProblemCapabilityMissing, issue.Name)
		}
		if strings.TrimSpace(reason) == "" {
			reason = text.T(i18n.ResumeReasonStillMissing)
		}
		if strings.TrimSpace(recommendation) == "" {
			recommendation = text.T(i18n.ResumeAdviceMeetOrChoose)
		}
		if len(issue.Options) == 0 {
			issue.Options = []RecoveryOption{{ID: "choose-node", Label: text.T(i18n.ResumeOptionChooseNode), Recommended: true}, {ID: "retry-checks", Label: text.T(i18n.ResumeOptionRetryAfterFix)}, {ID: "wait", Label: text.T(i18n.ResumeOptionWait)}}
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
