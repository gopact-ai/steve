package attempt

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/ledger"
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
	reopened.now = s.now
	same, exists, err := reopened.ForceRestart(t.Context(), r.Node)
	if err != nil || !exists || same != op {
		t.Fatalf("restart not durable: %+v %v", same, err)
	}
	first, err := reopened.ClaimForceRestart(t.Context(), op, "plan", "restart", allowRestart)
	if err != nil || !first {
		t.Fatalf("first claim: %v %v", first, err)
	}
	second, err := reopened.ClaimForceRestart(t.Context(), op, "another-plan", "restart", allowRestart)
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
		if ok, err := s.ClaimForceRestart(t.Context(), wrong, "plan", "restart", allowRestart); err == nil || ok {
			t.Fatalf("claimed another identity: %+v %v", wrong, err)
		}
	}
	if _, err = s.RequestForceStop(t.Context(), r.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RecordForceStopResult(t.Context(), r.ID, 2, true, "unavailable"); err != nil {
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
			yes, err := s.ClaimForceRestart(t.Context(), op, "plan", "restart", allowRestart)
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
	if yes, err := s.ClaimForceRestart(ctx, op, "plan", "restart", allowRestart); yes || err == nil {
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
	if yes, err := s.ClaimForceRestart(t.Context(), op, "plan", "restart", allowRestart); yes || err == nil {
		t.Fatal("expired operation launched SSH")
	}
}

func allowRestart(ledger.Reader, ForceRestart) error { return nil }

type restartReplication struct {
	book             *ledger.Ledger
	fail             bool
	entered, release chan struct{}
}

func (r *restartReplication) Prepare(context.Context) (ledger.ReplicaPosition, error) {
	v, err := r.book.ReplicaVersion()
	return ledger.ReplicaPosition{Version: v, CoordinatorEpoch: 1}, err
}
func (r *restartReplication) Propose(ctx context.Context, w ledger.ReplicatedWrite) ([]byte, error) {
	if r.entered != nil {
		close(r.entered)
		select {
		case <-r.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if r.fail {
		return nil, errors.New("replication refused")
	}
	return r.book.ApplyReplicated(w.ID, w.ExpectedVersion+1, w.Payload)
}

func TestForceRestartRefusedProposalRollsBackReservationAndClaim(t *testing.T) {
	for _, stage := range []string{"reserve", "claim"} {
		t.Run(stage, func(t *testing.T) {
			s, r := restartFixture(t)
			var op ForceRestart
			var err error
			if stage == "claim" {
				op, _, err = s.BeginForceRestart(t.Context(), r.ID, 1, "cluster", "holder")
				if err != nil {
					t.Fatal(err)
				}
			}
			replica := &restartReplication{book: s.l, fail: true}
			if err := s.l.AttachReplication(replica); err != nil {
				t.Fatal(err)
			}
			if stage == "reserve" {
				_, _, err = s.BeginForceRestart(t.Context(), r.ID, 1, "cluster", "holder")
			} else {
				_, err = s.ClaimForceRestart(t.Context(), op, "plan", "restart", allowRestart)
			}
			if err == nil {
				t.Fatal("refused proposal reported success")
			}
			got, found, readErr := s.ForceRestart(t.Context(), r.Node)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if stage == "reserve" && found {
				t.Fatal("refused reservation published operation")
			}
			if stage == "claim" && (!found || !got.ClaimedAt.IsZero()) {
				t.Fatal("refused claim consumed execution right")
			}
		})
	}
}

func TestForceRestartClaimIsInvisibleUntilReplicationCompletes(t *testing.T) {
	s, r := restartFixture(t)
	op, _, err := s.BeginForceRestart(t.Context(), r.ID, 1, "cluster", "holder")
	if err != nil {
		t.Fatal(err)
	}
	replica := &restartReplication{book: s.l, entered: make(chan struct{}), release: make(chan struct{})}
	if err := s.l.AttachReplication(replica); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		yes, err := s.ClaimForceRestart(ctx, op, "plan", "restart", allowRestart)
		if err == nil && !yes {
			err = errors.New("claim not won")
		}
		done <- err
	}()
	select {
	case <-replica.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	current, _, err := s.ForceRestart(ctx, r.Node)
	if err != nil || !current.ClaimedAt.IsZero() {
		close(replica.release)
		<-done
		t.Fatalf("uncommitted claim became visible: %+v %v", current, err)
	}
	close(replica.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	restored := New(s.l)
	restored.now = s.now
	if yes, err := restored.ClaimForceRestart(ctx, op, "second", "restart", allowRestart); err != nil || yes {
		t.Fatalf("lost response may replay SSH: %v %v", yes, err)
	}
}

func TestForceRestartTerminalOperationNeedsANewExplicitRequest(t *testing.T) {
	s, r := restartFixture(t)
	op, _, err := s.BeginForceRestart(t.Context(), r.ID, 1, "cluster", "holder")
	if err != nil {
		t.Fatal(err)
	}
	now := op.RequestedAt.Add(time.Second)
	s.now = func() time.Time { return now }
	if err := s.FinishForceRestart(t.Context(), op, "lost"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.BeginForceRestart(t.Context(), r.ID, 1, "cluster", "new-holder"); err != nil {
		t.Fatal(err)
	}
	same, _, _ := s.ForceRestart(t.Context(), r.Node)
	if same.ID != op.ID {
		t.Fatal("poll created a new operation")
	}
	now = now.Add(time.Second)
	if _, err := s.RequestForceStop(t.Context(), r.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordForceStopResult(t.Context(), r.ID, 2, true, "unavailable"); err != nil {
		t.Fatal(err)
	}
	next, fresh, err := s.BeginForceRestart(t.Context(), r.ID, 2, "cluster", "new-holder")
	if err != nil || !fresh || next.ID == op.ID {
		t.Fatalf("new explicit request not reserved: %+v %v", next, err)
	}
	if yes, err := s.ClaimForceRestart(t.Context(), op, "late", "restart", allowRestart); yes || err == nil {
		t.Fatal("late operation claimed after replacement")
	}
	if err := s.FinishForceRestart(t.Context(), op, "connected"); err == nil {
		t.Fatal("old result changed replacement")
	}
}

func TestForceRestartExpiredUnobservedOperationDoesNotTrapANewRequest(t *testing.T) {
	s, r := restartFixture(t)
	op, _, err := s.BeginForceRestart(t.Context(), r.ID, 1, "cluster", "holder")
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return op.RequestedAt.Add(8 * time.Minute) }
	if _, err := s.RequestForceStop(t.Context(), r.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordForceStopResult(t.Context(), r.ID, 2, true, "unavailable"); err != nil {
		t.Fatal(err)
	}
	next, fresh, err := s.BeginForceRestart(t.Context(), r.ID, 2, "cluster", "holder")
	if err != nil || !fresh || next.ID == op.ID {
		t.Fatalf("explicit new request trapped behind expired operation: %+v %v %v", next, fresh, err)
	}
	if yes, err := s.ClaimForceRestart(t.Context(), op, "late", "restart", allowRestart); yes || err == nil {
		t.Fatal("expired replaced operation was claimed")
	}
}
