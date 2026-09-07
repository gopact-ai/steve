package checkpoint

import (
	"context"
	"testing"
)

func safeResumeRequest(t *testing.T) ResumeRequest {
	t.Helper()
	s := newTestStore(t, "node-b", &recordingRecords{}, nil, testPolicy{}, Limits{})
	snapshot := testSnapshot(t, s, 1)
	m, err := s.Commit(context.Background(), snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := s.Verify(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	return ResumeRequest{Source: snapshot.Source, Checkpoint: &verified,
		Isolation:      IsolationEvidence{AttemptID: snapshot.Source.AttemptID, ExecutionEpoch: 3, Kind: IsolationProcessStopped, Reference: "node process-exit record #8"},
		Reconciliation: ActionReconciliation{AttemptID: snapshot.Source.AttemptID, ExecutionEpoch: 3, Checked: true, IsolationReference: "node process-exit record #8"},
		Target:         ResumeTarget{NodeID: "node-b", ExecutionEpoch: 4, Authorized: true, AdmissionChecked: true}}
}

func TestResumePrefersAttachedAuthorizedAttempt(t *testing.T) {
	source := Source{TaskID: "task-1", SessionID: "conversation-1", AttemptID: "attempt-1", TurnID: "turn-1", NodeID: "node-a", ExecutionEpoch: 3}
	plan := ResumePlan(ResumeRequest{Source: source, Live: &LiveAttempt{Source: source, Attached: true, AuthorityValid: true, Cursors: Cursors{InputAccepted: 5, OutputPublished: 12}}})
	if plan.Action != ResumeReattach || plan.NodeID != "node-a" || plan.AttemptID != "attempt-1" || plan.Cursors.OutputPublished != 12 {
		t.Fatalf("live attempt was replaced: %+v", plan)
	}
}

func TestResumeNeverTreatsTimeoutOrEpochAloneAsWriterIsolation(t *testing.T) {
	req := safeResumeRequest(t)
	for _, evidence := range []IsolationEvidence{
		{},
		{AttemptID: req.Source.AttemptID, ExecutionEpoch: 3, Kind: "heartbeat-timeout", Reference: "no heartbeat for 90 seconds"},
		{AttemptID: req.Source.AttemptID, ExecutionEpoch: 2, Kind: IsolationResourcesFenced, Reference: "storage fence #2"},
	} {
		req.Isolation = evidence
		plan := ResumePlan(req)
		if plan.Action != ResumeAskUser || plan.Question == nil || plan.Question.Code != "writer-not-isolated" {
			t.Fatalf("unsafe replacement allowed: %+v", plan)
		}
	}
}

func TestResumeChecksPostCheckpointExternalActionsAndCapabilities(t *testing.T) {
	req := safeResumeRequest(t)
	req.Reconciliation.Checked = false
	if plan := ResumePlan(req); plan.Action != ResumeAskUser || plan.Question.Code != "actions-not-reconciled" {
		t.Fatalf("unchecked post-checkpoint effects replayed: %+v", plan)
	}
	req.Reconciliation.Checked = true
	req.Reconciliation.AdditionalUnknown = []ExternalAction{{ID: "action-9", Description: "create remote change request", ReconcileRef: "operation/action-9"}}
	if plan := ResumePlan(req); plan.Action != ResumeAskUser || plan.Question.Code != "action-result-unknown" {
		t.Fatalf("unknown side effect replayed: %+v", plan)
	}
	req.Reconciliation.Results = []ActionResolution{{ActionID: "action-9", Outcome: ActionApplied, Evidence: "change request #42 exists"}}
	req.Target.Missing = []CapabilityIssue{{Kind: "credential", Name: "project network", Attempted: []string{"已检查目标节点的网络连通性"}, Problem: "目标节点无法访问项目内网", Reason: "目标节点尚未登录项目网络", Recommendation: "在目标节点完成登录后继续验证。", Options: []RecoveryOption{{ID: "sign-in", Label: "我去完成登录", Recommended: true}, {ID: "choose-node", Label: "选择其他机器"}}}}
	plan := ResumePlan(req)
	if plan.Action != ResumeAskUser || plan.Question == nil || plan.Question.Problem != "目标节点无法访问项目内网" || len(plan.Question.Attempted) == 0 || plan.Question.Recommendation == "" || !plan.Question.AllowFreeText {
		t.Fatalf("capability refusal lacks useful Ask User context: %+v", plan)
	}
	req.Target.Missing = nil
	plan = ResumePlan(req)
	if plan.Action != ResumeStartAttempt || plan.TaskID != req.Source.TaskID || plan.SessionID != req.Source.SessionID || plan.PreviousAttemptID != req.Source.AttemptID || plan.AttemptID != "" || plan.ExecutionEpoch != 4 || len(plan.ReconciledActions) != 1 {
		t.Fatalf("invalid recovery plan: %+v", plan)
	}
}

func TestResumeRejectsWrongCheckpointOrStaleTarget(t *testing.T) {
	req := safeResumeRequest(t)
	req.Source.SessionID = "other-session"
	if plan := ResumePlan(req); plan.Action != ResumeAskUser || plan.Question.Code != "checkpoint-mismatch" {
		t.Fatalf("unrelated session checkpoint reused: %+v", plan)
	}
	req = safeResumeRequest(t)
	req.Target.ExecutionEpoch = req.Source.ExecutionEpoch
	if plan := ResumePlan(req); plan.Action != ResumeAskUser {
		t.Fatalf("old writer epoch reused: %+v", plan)
	}
}
