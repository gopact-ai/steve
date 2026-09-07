package attempt

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/checkpoint"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/project"
	"github.com/gopact-ai/steve/internal/task"
)

func relocationFixture(t *testing.T) (*Service, *clock, Record, RelocationIntent, RetainedEvidence, *task.Store) {
	s, now, old, proof, tasks := retainedFixture(t)
	p := RelocationIntent{SourceID: old.ID, SourceRevision: old.Revision, TaskEpoch: old.Execution.Epoch, Checkpoint: "complete-checkpoint", Owner: "owner", CreatedAt: now.t, InputDigest: "input-digest", Prompt: "continue original task", UnknownActions: []checkpoint.ExternalAction{{ID: "opaque-cli", Description: "original CLI request outcome is unknown"}}, Target: Spec{ID: "att-replacement", TaskID: old.TaskID, TurnID: old.TurnID, Kind: KindChat, Project: old.Project, Node: "node-b", Harness: old.Harness, Agent: old.Agent, Execution: old.Execution, ExecutionGeneration: SessionExecutionEpoch(old) + 1, NativeCommandID: "new-input-command", Scope: ScopePathSet, Base: "complete-checkpoint", Workspace: project.Workspace{ID: "replacement", Project: old.Project, Node: "node-b", Path: "/isolated/replacement", Kind: project.KindWorktree}}}
	var err error
	p, err = s.RecordRelocation(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	return s, now, old, p, proof, tasks
}

func TestRelocationPreparationFreezesMCPConfigurationBeforeAnUncertainOpen(t *testing.T) {
	s, _, _, plan, _, _ := relocationFixture(t)
	r, err := s.OpenRelocation(t.Context(), plan.ID, manualRelocation(plan))
	if err != nil {
		t.Fatal(err)
	}
	config := RelocationSessionConfig{MCPServers: []acp.MCPServer{acp.HTTPMCPServer("tool", "http://127.0.0.1:9000/b/original-binding", nil), acp.StdioMCPServer("stdio", "steve-node", []string{"--binding", "fixed-binding"}, nil)}, AgentToken: "test-native-token", Fingerprint: "capabilities", Instructions: "original instructions"}
	if err := s.RecordRelocationSession(t.Context(), r.ID, config); err != nil {
		t.Fatal(err)
	}
	// The native open completed but its response never reached the old
	// coordinator: no Session ID has yet been committed to this attempt.
	if _, err := s.PrepareRecovery(t.Context(), "new coordinator"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecoverRelocationPreparation(t.Context(), r.ID); err != nil {
		t.Fatal(err)
	}
	restarted := New(s.l)
	frozen, found, err := restarted.RelocationSession(t.Context(), r.ID)
	if err != nil || !found {
		t.Fatalf("native open payload was not recoverable: %v", err)
	}
	want, _ := json.Marshal(config)
	got, _ := json.Marshal(frozen)
	if string(got) != string(want) {
		t.Fatal("recovered native open changed its exact MCP configuration")
	}
	config.MCPServers = []acp.MCPServer{acp.HTTPMCPServer("tool", "http://127.0.0.1:9000/b/rebound-id", nil)}
	if err := restarted.RecordRelocationSession(t.Context(), r.ID, config); err == nil {
		t.Fatal("retry replaced the original binding behind the same open command")
	}
}

func manualRelocation(p RelocationIntent) RelocationApproval {
	choice := "confirm-stopped-and-retry:" + p.ID
	a := RelocationApproval{PlanID: p.ID, Actor: p.Owner, ChoiceID: choice, StoppedConfirmed: true, EffectsReviewed: true}
	for _, action := range p.UnknownActions {
		a.ActionResults = append(a.ActionResults, checkpoint.ActionResolution{ActionID: action.ID, Outcome: checkpoint.ActionRetryAuthorized, Evidence: choice})
	}
	return a
}

func TestRelocationApprovalIsBoundToExactPlanAndKeepsLogicalTurn(t *testing.T) {
	s, _, old, p, _, _ := relocationFixture(t)
	if _, err := s.OpenRelocation(t.Context(), p.ID, RelocationApproval{}); err == nil {
		t.Fatal("absence of effects was treated as approval")
	}
	a := manualRelocation(p)
	a.ChoiceID = "confirm-stopped-and-retry:other-plan"
	if _, err := s.OpenRelocation(t.Context(), p.ID, a); err == nil {
		t.Fatal("another plan authorized this relocation")
	}
	a = manualRelocation(p)
	created, err := s.OpenRelocation(t.Context(), p.ID, a)
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == old.ID || created.TaskID != old.TaskID || created.TurnID != old.TurnID || *created.Execution != *old.Execution || created.Workspace.Node == old.Node || created.Recovery == nil || created.Recovery.PlanID != p.ID {
		t.Fatalf("relocation changed original task or authority: %+v", created)
	}
	if InputCommandID(created) == InputCommandID(old) || SessionExecutionEpoch(created) <= SessionExecutionEpoch(old) {
		t.Fatal("new native command reused old input identity")
	}
	retired, _ := s.Get(t.Context(), old.ID)
	if retired.State != Superseded || retired.SupersededBy != created.ID || retired.Unsettled {
		t.Fatalf("old execution not retired: %+v", retired)
	}
	if err := s.Renew(t.Context(), old.ID); err == nil {
		t.Fatal("old writer can renew after relocation")
	}
	again, err := s.OpenRelocation(t.Context(), p.ID, a)
	if err != nil || again.ID != created.ID {
		t.Fatalf("retry duplicated relocation: %+v %v", again, err)
	}
}

func TestApprovedRelocationPreparationSurvivesCoordinatorLossWithoutNewAttempt(t *testing.T) {
	s, now, _, p, _, _ := relocationFixture(t)
	created, err := s.OpenRelocation(t.Context(), p.ID, manualRelocation(p))
	if err != nil {
		t.Fatal(err)
	}
	created, err = s.Advance(t.Context(), created.ID, Prepared, "prepare", nil)
	if err != nil {
		t.Fatal(err)
	}
	created, err = s.RecordSession(t.Context(), created.ID, "prepare", "ns_existing_preparation")
	if err != nil {
		t.Fatal(err)
	}
	now.t = now.t.Add(time.Hour)
	if _, err := s.PrepareRecovery(t.Context(), "new coordinator"); err != nil {
		t.Fatal(err)
	}
	prepared, err := s.Get(t.Context(), created.ID)
	if err != nil || prepared.State != Prepared || !prepared.Unsettled {
		t.Fatalf("preparation discarded: %+v %v", prepared, err)
	}
	adopted, err := s.RecoverRelocationPreparation(t.Context(), created.ID)
	if err != nil || adopted.ID != created.ID || adopted.Session != created.Session || adopted.State != Prepared || adopted.Unsettled || adopted.Leases[0].Epoch != created.Leases[0].Epoch {
		t.Fatalf("preparation replaced its original execution: %+v %v", adopted, err)
	}
	if _, err = s.Advance(t.Context(), created.ID, Running, "dispatch", nil); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RecoverRelocationPreparation(t.Context(), created.ID); err == nil {
		t.Fatal("running command admitted as undispatched preparation")
	}
}

func TestRelocationRollsBackOldRetirementWhenReplacementLeaseCannotBeAcquired(t *testing.T) {
	s, _, old, p, _, _ := relocationFixture(t)
	if _, err := s.l.Acquire(t.Context(), "workspace:"+p.Target.Workspace.ID, "another-writer", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenRelocation(t.Context(), p.ID, manualRelocation(p)); !errors.Is(err, ledger.ErrHeld) {
		t.Fatalf("expected destination conflict, got %v", err)
	}
	current, _ := s.Get(t.Context(), old.ID)
	if current.State != old.State || current.Revision != old.Revision {
		t.Fatal("failed relocation partially retired original attempt")
	}
	if err := s.Renew(t.Context(), old.ID); err != nil {
		t.Fatalf("failed relocation cut original fences: %v", err)
	}
	if _, err := s.Get(t.Context(), p.Target.ID); err == nil {
		t.Fatal("partial new attempt escaped transaction")
	}
}

func TestRelocationRequiresTaskAuthorizationAndRealUndispatchedProof(t *testing.T) {
	t.Run("task stopped", func(t *testing.T) {
		s, _, old, p, _, tasks := relocationFixture(t)
		if _, err := tasks.SetAside(old.TaskID, task.StatePaused); err != nil {
			t.Fatal(err)
		}
		if _, err := s.OpenRelocation(t.Context(), p.ID, manualRelocation(p)); !errors.Is(err, task.ErrExecutionStopped) {
			t.Fatalf("revoked task relocated: %v", err)
		}
	})
	for _, marker := range []string{"", "dispatched", "not-dispatched"} {
		t.Run(marker, func(t *testing.T) {
			s, _, _, p, proof, _ := relocationFixture(t)
			p.ID = ""
			p.UnknownActions = nil
			var err error
			p, err = s.RecordRelocation(t.Context(), p)
			if err != nil {
				t.Fatal(err)
			}
			proof.Session.State = "interrupted"
			proof.Session.ProcessStopped = true
			proof.Session.Command.ProcessStopped = true
			proof.Session.Command.State = "uncertain"
			proof.Session.Command.DispatchState = marker
			_, err = s.OpenRelocation(t.Context(), p.ID, RelocationApproval{Node: &proof})
			if (marker == "not-dispatched") != (err == nil) {
				t.Fatalf("dispatch %q: %v", marker, err)
			}
		})
	}
}
