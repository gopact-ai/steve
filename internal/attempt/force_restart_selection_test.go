package attempt

import "testing"

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
