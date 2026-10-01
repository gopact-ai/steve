package attempt

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/task"
)

func restartFixture(t *testing.T) (*Service, Record) {
	t.Helper()
	s, _, r, _, tasks := retainedFixture(t)
	if _, err := tasks.SetAside(r.TaskID, task.StateCancelled); err != nil {
		t.Fatal(err)
	}
	r, err := s.RequestForceStop(t.Context(), r.ID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.RecordForceStopResult(t.Context(), r.ID, r.ForceStop.Revision, true, "unavailable")
	if err != nil {
		t.Fatal(err)
	}
	return s, r
}

func TestForceRestartIsPersistedBeforeDispatchAndClaimedOnce(t *testing.T) {
	s, r := restartFixture(t)
	op, fresh, err := s.BeginForceRestart(t.Context(), r.ID, r.ForceStop.Revision, "cluster", "holder")
	if err != nil || !fresh || op.ID == "" || op.NodeID != r.Node || op.Holder != "holder" {
		t.Fatalf("restart not reserved: %+v fresh=%v err=%v", op, fresh, err)
	}
	got, _ := s.Get(t.Context(), r.ID)
	if got.ForceStop.Level != "restart" {
		t.Fatalf("attempt not waiting on restart: %+v", got.ForceStop)
	}
	reopened := New(s.l)
	same, exists, err := reopened.ForceRestart(t.Context(), r.Node)
	if err != nil || !exists || same != op {
		t.Fatalf("restart not durable: %+v %v", same, err)
	}
	first, err := reopened.ClaimForceRestart(t.Context(), op, "plan", "restart")
	if err != nil || !first {
		t.Fatalf("first claim: %v %v", first, err)
	}
	second, err := reopened.ClaimForceRestart(t.Context(), op, "another-plan", "restart")
	if err != nil || second {
		t.Fatalf("replayed claim can start SSH: %v %v", second, err)
	}
	same, _, _ = reopened.ForceRestart(t.Context(), r.Node)
	if same.PlanID != "plan" || same.ClaimedAt.IsZero() {
		t.Fatalf("replay replaced the restart: %+v", same)
	}
}

func TestForceRestartRejectsWrongIdentityAndOldRevision(t *testing.T) {
	s, r := restartFixture(t)
	op, _, err := s.BeginForceRestart(t.Context(), r.ID, r.ForceStop.Revision, "cluster", "holder")
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*ForceRestart){func(v *ForceRestart) { v.ID = "other" }, func(v *ForceRestart) { v.NodeID = "other" }, func(v *ForceRestart) { v.ClusterID = "other" }, func(v *ForceRestart) { v.Holder = "other" }, func(v *ForceRestart) { v.By = "other" }} {
		wrong := op
		change(&wrong)
		if ok, err := s.ClaimForceRestart(t.Context(), wrong, "plan", "restart"); err == nil || ok {
			t.Fatalf("claimed another identity: %+v %v", wrong, err)
		}
	}
	if _, err = s.RequestForceStop(t.Context(), r.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.BeginForceRestart(t.Context(), r.ID, 1, "cluster", "holder"); !errors.Is(err, ErrForceStopChanged) {
		t.Fatalf("old revision reserved restart: %v", err)
	}
}

func TestForceRestartConcurrentClaimsHaveOneWinner(t *testing.T) {
	s, r := restartFixture(t)
	op, _, err := s.BeginForceRestart(t.Context(), r.ID, 1, "cluster", "holder")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan bool, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			yes, err := s.ClaimForceRestart(t.Context(), op, "plan", "restart")
			if err != nil {
				t.Error(err)
			}
			results <- yes
		}()
	}
	wg.Wait()
	close(results)
	wins := 0
	for yes := range results {
		if yes {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("claim winners=%d", wins)
	}
}

func TestForceRestartCancellationCannotReserveOrClaim(t *testing.T) {
	s, r := restartFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := s.BeginForceRestart(ctx, r.ID, 1, "cluster", "holder"); err == nil {
		t.Fatal("cancelled reservation succeeded")
	}
	op, _, err := s.BeginForceRestart(t.Context(), r.ID, 1, "cluster", "holder")
	if err != nil {
		t.Fatal(err)
	}
	if yes, err := s.ClaimForceRestart(ctx, op, "plan", "restart"); yes || err == nil {
		t.Fatal("cancelled claim succeeded")
	}
	got, _, _ := s.ForceRestart(t.Context(), r.Node)
	if !got.ClaimedAt.IsZero() {
		t.Fatal("cancelled claim changed the operation")
	}
}

func TestForceRestartTimeoutDoesNotResetItsClock(t *testing.T) {
	s, r := restartFixture(t)
	op, fresh, err := s.BeginForceRestart(t.Context(), r.ID, 1, "cluster", "holder")
	if err != nil || !fresh {
		t.Fatalf("reserve: %v", err)
	}
	s.now = func() time.Time { return op.RequestedAt.Add(8 * time.Minute) }
	again, fresh, err := s.BeginForceRestart(t.Context(), r.ID, 1, "cluster", "another-holder")
	if err != nil || fresh || again != op {
		t.Fatalf("poll reset or relocated restart: %+v %v %v", again, fresh, err)
	}
	if yes, err := s.ClaimForceRestart(t.Context(), op, "plan", "restart"); yes || err == nil {
		t.Fatal("expired operation launched SSH")
	}
}
