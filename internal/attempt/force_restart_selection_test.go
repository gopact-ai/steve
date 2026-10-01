package attempt

import (
	"testing"
	"time"
)

func TestForceRestartSelectedPlanIsPartOfTheClaimIdentity(t *testing.T) {
	s, r := restartFixture(t)
	selection := ForceRestartSelection{MembershipRevision: 4, JoinPlanID: "existing-upgrade", JoinKind: "upgrade"}
	op, _, err := s.BeginForceRestart(t.Context(), r.ID, 1, "cluster", "holder", selection)
	if err != nil {
		t.Fatal(err)
	}
	for _, wrong := range []struct{ plan, kind string }{{"different", "upgrade"}, {"existing-upgrade", "restart"}} {
		if claimed, err := s.ClaimForceRestart(t.Context(), op, wrong.plan, wrong.kind, allowRestart); err == nil || claimed {
			t.Fatalf("unobserved local plan claimed: %s %s %v %v", wrong.plan, wrong.kind, claimed, err)
		}
	}
	changed := op
	changed.Selection.JoinPlanID = "different"
	if claimed, err := s.ClaimForceRestart(t.Context(), changed, "different", "upgrade", allowRestart); err == nil || claimed {
		t.Fatal("modified discovery choice replaced the durable one")
	}
	if claimed, err := s.ClaimForceRestart(t.Context(), op, "existing-upgrade", "upgrade", allowRestart); err != nil || !claimed {
		t.Fatalf("exact observed plan not joined: %v %v", claimed, err)
	}
}

func TestForceRestartWithoutAnExactSelectionCannotClaimOrVerify(t *testing.T) {
	for _, selection := range []ForceRestartSelection{{}, {MembershipRevision: 1, JoinKind: "restart"}, {MembershipRevision: 1, JoinPlanID: "plan"}, {MembershipRevision: 1, JoinPlanID: "plan", JoinKind: "unknown"}} {
		s, r := restartFixture(t)
		op := ForceRestart{ID: "incomplete-selection", ClusterID: "cluster", NodeID: r.Node, Holder: "holder", By: "owner", RequestedAt: r.ForceStop.LevelSince, Selection: selection}
		if err := s.l.PutBinding(t.Context(), forceRestartKind, r.Node, op); err != nil {
			t.Fatal(err)
		}
		if claimed, err := s.ClaimForceRestart(t.Context(), op, "plan", "restart", allowRestart); err == nil || claimed {
			t.Fatalf("incomplete selection granted script execution: %+v %v %v", selection, claimed, err)
		}
		op.ClaimedAt = r.ForceStop.LevelSince.Add(time.Second)
		op.PlanID = "plan"
		op.Kind = "restart"
		if err := s.l.PutBinding(t.Context(), forceRestartKind, r.Node, op); err != nil {
			t.Fatal(err)
		}
		if err := s.VerifyForceRestart(t.Context(), op, "plan", "restart", allowRestart); err == nil {
			t.Fatalf("incomplete historical selection passed execution verification: %+v", selection)
		}
	}
}
