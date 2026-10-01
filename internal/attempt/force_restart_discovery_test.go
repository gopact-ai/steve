package attempt

import (
	"context"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
)

func TestForceRestartReservationRetainsTheDiscoveryBudget(t *testing.T) {
	s, r := restartFixture(t)
	start := r.ForceStop.LevelSince
	clock := start.Add(6 * time.Minute)
	s.now = func() time.Time { return clock }
	op, fresh, err := s.BeginForceRestart(t.Context(), r.ID, 1, "cluster", "holder")
	if err != nil || !fresh {
		t.Fatalf("reserve: fresh=%v err=%v", fresh, err)
	}
	got, err := s.Get(t.Context(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !op.RequestedAt.Equal(start) || !got.ForceStop.LevelSince.Equal(start) || !got.ForceStop.RestartRequestedAt.Equal(start) {
		t.Fatalf("discovery reset the restart budget: start=%s operation=%s state=%+v", start, op.RequestedAt, got.ForceStop)
	}
	if yes, err := s.ClaimForceRestart(t.Context(), op, "plan", "restart", allowRestart); err != nil || !yes {
		t.Fatalf("claim before deadline: %v %v", yes, err)
	}
	clock = start.Add(7 * time.Minute)
	if err := s.VerifyForceRestart(t.Context(), op, "plan", "restart", allowRestart); err == nil {
		t.Fatal("discovery extended execution authorization beyond the phase deadline")
	}
}

func TestForceRestartDiscoveryCannotReserveAfterItsDeadline(t *testing.T) {
	s, r := restartFixture(t)
	s.now = func() time.Time { return r.ForceStop.LevelSince.Add(7 * time.Minute) }
	if _, fresh, err := s.BeginForceRestart(t.Context(), r.ID, 1, "cluster", "holder"); err == nil || fresh {
		t.Fatalf("expired discovery reserved an operation: fresh=%v err=%v", fresh, err)
	}
	if _, found, err := s.ForceRestart(t.Context(), r.Node); err != nil || found {
		t.Fatalf("expired discovery changed operations: found=%v err=%v", found, err)
	}
	got, err := s.Get(t.Context(), r.ID)
	if err != nil || got.Revision != r.Revision || got.ForceStop.RestartID != "" {
		t.Fatalf("expired reservation partially changed attempt: %+v %v", got, err)
	}
}

type discoveryPreparation struct {
	*restartReplication
	advance func()
}

func (r *discoveryPreparation) Prepare(ctx context.Context) (ledger.ReplicaPosition, error) {
	if r.advance != nil {
		r.advance()
	}
	return r.restartReplication.Prepare(ctx)
}

func TestForceRestartDiscoveryDeadlineIsCheckedInsideReservation(t *testing.T) {
	s, r := restartFixture(t)
	clock := r.ForceStop.LevelSince.Add(7*time.Minute - time.Millisecond)
	s.now = func() time.Time { return clock }
	replica := &discoveryPreparation{restartReplication: &restartReplication{book: s.l}}
	if err := s.l.AttachReplication(replica); err != nil {
		t.Fatal(err)
	}
	replica.advance = func() { clock = r.ForceStop.LevelSince.Add(7 * time.Minute) }
	if _, fresh, err := s.BeginForceRestart(t.Context(), r.ID, 1, "cluster", "holder"); err == nil || fresh {
		t.Fatalf("reservation prepared after the deadline: fresh=%v err=%v", fresh, err)
	}
	if _, found, err := s.ForceRestart(t.Context(), r.Node); err != nil || found {
		t.Fatalf("late reservation published: found=%v err=%v", found, err)
	}
}

func TestForceRestartJoiningKeepsTheEarlierDeadline(t *testing.T) {
	s, r := restartFixture(t)
	op, _, err := s.BeginForceRestart(t.Context(), r.ID, 1, "cluster", "holder")
	if err != nil {
		t.Fatal(err)
	}
	clock := op.RequestedAt.Add(5 * time.Minute)
	s.now = func() time.Time { return clock }
	if _, err := s.RequestForceStop(t.Context(), r.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordForceStopResult(t.Context(), r.ID, 2, true, "unavailable"); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(time.Minute)
	joined, fresh, err := s.BeginForceRestart(t.Context(), r.ID, 2, "cluster", "another-holder")
	if err != nil || fresh || joined.ID != op.ID || !joined.RequestedAt.Equal(op.RequestedAt) {
		t.Fatalf("join extended or replaced the operation: %+v %v %v", joined, fresh, err)
	}
	got, err := s.Get(t.Context(), r.ID)
	if err != nil || !got.ForceStop.LevelSince.Equal(op.RequestedAt) {
		t.Fatalf("join lost the earlier deadline: %+v %v", got.ForceStop, err)
	}
}

func TestForceRestartEarlyDiscoveryDoesNotExtendWhenJoiningALaterOperation(t *testing.T) {
	s, r := restartFixture(t)
	start := r.ForceStop.LevelSince
	op := ForceRestart{ID: "shared-operation", ClusterID: "cluster", NodeID: r.Node, Holder: "holder", By: "owner", RequestedAt: start.Add(time.Minute)}
	s.now = func() time.Time { return start.Add(2 * time.Minute) }
	if err := s.l.PutBinding(t.Context(), forceRestartKind, r.Node, op); err != nil {
		t.Fatal(err)
	}
	joined, fresh, err := s.BeginForceRestart(t.Context(), r.ID, 1, "cluster", "another-holder")
	if err != nil || fresh || joined != op {
		t.Fatalf("join changed shared operation: %+v %v %v", joined, fresh, err)
	}
	got, err := s.Get(t.Context(), r.ID)
	if err != nil || !got.ForceStop.LevelSince.Equal(start) || !got.ForceStop.RestartRequestedAt.Equal(op.RequestedAt) {
		t.Fatalf("joining a later operation extended discovery: %+v %v", got.ForceStop, err)
	}
	current, _, err := s.ForceRestart(t.Context(), r.Node)
	if err != nil || current != op {
		t.Fatalf("join rewrote operation identity: %+v %v", current, err)
	}
}
