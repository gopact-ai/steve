package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/attempt"
	"github.com/gopact-ai/steve/internal/cluster"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/node"
	"github.com/gopact-ai/steve/internal/sshconnect"
)

type forceRestartHost struct {
	mu        sync.Mutex
	holder    string
	failure   error
	starts    []attempt.ForceRestart
	status    sshconnect.MemberRestart
	attempts  *attempt.Service
	loseReply bool
}

func (h *forceRestartHost) Find(_ context.Context, node, by string) (cluster.RestartTarget, error) {
	if node == "hub" {
		return cluster.RestartTarget{}, cluster.MemberRestartError{Reason: "restart_self"}
	}
	if by != "owner" {
		return cluster.RestartTarget{}, cluster.MemberRestartError{Reason: "restart_permission"}
	}
	if h.failure != nil {
		return cluster.RestartTarget{}, h.failure
	}
	return cluster.RestartTarget{ClusterID: "cluster", Holder: h.holder}, nil
}
func (h *forceRestartHost) Start(ctx context.Context, op attempt.ForceRestart) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	yes, err := h.attempts.ClaimForceRestart(ctx, op, "plan", "restart", func(ledger.Reader, attempt.ForceRestart) error { return nil })
	if err != nil {
		return err
	}
	if yes {
		h.starts = append(h.starts, op)
	}
	if h.loseReply {
		return errors.New("reply lost")
	}
	return nil
}
func (h *forceRestartHost) Status(_ context.Context, op attempt.ForceRestart) (sshconnect.MemberRestart, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	result := h.status
	result.NodeID, result.RequestID = op.NodeID, op.ID
	return result, nil
}

func forceRestartFixture(t *testing.T, n int) (*applicationStops, []attempt.Record, *forceSessions, *forceRestartHost) {
	s, records, sessions := forceFixture(t, n)
	host := &forceRestartHost{holder: "link-owner", attempts: s.attempts, status: sshconnect.MemberRestart{State: "running", PlanID: "plan", Kind: "restart"}}
	s.restarts = host
	sessions.failure = &node.SessionError{Code: "unavailable", Message: "original host unavailable"}
	for i, r := range records {
		var err error
		records[i], err = s.attempts.RequestForceStop(t.Context(), r.ID, "owner")
		if err != nil {
			t.Fatal(err)
		}
	}
	return s, records, sessions, host
}

func TestForceRestartRoutesThroughHolderThenConfirmsOnlyOnKill(t *testing.T) {
	s, records, sessions, host := forceRestartFixture(t, 1)
	if err := s.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	r, _ := s.attempts.Get(t.Context(), records[0].ID)
	if len(host.starts) != 1 || r.ForceStop.Level != "restart" || r.ForceStop.RestartHolder != "link-owner" {
		t.Fatalf("restart not routed: %+v starts=%d", r.ForceStop, len(host.starts))
	}
	host.status.State = "connected"
	if err := s.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	r, _ = s.attempts.Get(t.Context(), r.ID)
	if r.ForceStop.Level != "await" || !r.Unsettled {
		t.Fatalf("SSH return falsely confirmed process: %+v", r)
	}
	sessions.failure = nil
	if err := s.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	r, _ = s.attempts.Get(t.Context(), r.ID)
	if r.ForceStop.Level != "confirmed" || r.Unsettled || !r.StopProjected {
		t.Fatalf("original exit not projected: %+v", r)
	}
}

func TestForceRestartMultipleAttemptsAndLostReplyNeverRepeatSSH(t *testing.T) {
	s, records, _, host := forceRestartFixture(t, 2)
	host.loseReply = true
	_ = s.Reconcile(t.Context())
	if len(host.starts) != 1 {
		t.Fatalf("same machine restarted %d times", len(host.starts))
	}
	for _, r := range records {
		got, _ := s.attempts.Get(t.Context(), r.ID)
		if got.ForceStop.Level != "restart" {
			t.Fatalf("lost reply lost durable phase: %+v", got.ForceStop)
		}
	}
	_ = s.Reconcile(t.Context())
	if len(host.starts) != 1 {
		t.Fatal("reconciliation replayed SSH")
	}
}

func TestForceRestartMissingStatusCannotSendAnotherRestart(t *testing.T) {
	s, records, _, host := forceRestartFixture(t, 1)
	if err := s.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	host.status.State = "lost"
	if err := s.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	r, _ := s.attempts.Get(t.Context(), records[0].ID)
	// An answered node can still be checked in L3, but never restarted again.
	if len(host.starts) != 1 || r.ForceStop.Level != "await" {
		t.Fatalf("lost status restarted or stranded an answering node: %+v starts=%d", r.ForceStop, len(host.starts))
	}
}

func TestForceRestartRefusesMissingHolderAndBoundsBothPhases(t *testing.T) {
	t.Run("missing holder", func(t *testing.T) {
		s, records, _, host := forceRestartFixture(t, 1)
		host.failure = cluster.MemberRestartError{Reason: "restart_no_holder"}
		if err := s.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
		r, _ := s.attempts.Get(t.Context(), records[0].ID)
		if r.ForceStop.Level != "exhausted" || r.ForceStop.Reason != "restart_no_holder" || len(host.starts) != 0 {
			t.Fatalf("missing holder=%+v", r.ForceStop)
		}
	})
	for _, phase := range []string{"restart", "await"} {
		t.Run(phase, func(t *testing.T) {
			s, records, _, host := forceRestartFixture(t, 1)
			if err := s.Reconcile(t.Context()); err != nil {
				t.Fatal(err)
			}
			if phase == "await" {
				host.status.State = "connected"
				if err := s.Reconcile(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			s.now = func() time.Time { return time.Now().Add(8 * time.Minute) }
			if err := s.Reconcile(t.Context()); err != nil {
				t.Fatal(err)
			}
			r, _ := s.attempts.Get(t.Context(), records[0].ID)
			if r.ForceStop.Level != "exhausted" || len(host.starts) != 1 {
				t.Fatalf("unbounded phase: %+v", r.ForceStop)
			}
		})
	}
}

func TestForceRestartNeverRestartsSelfOrUsesAnotherOwnersAuthority(t *testing.T) {
	for _, why := range []string{"restart_self", "restart_permission"} {
		t.Run(why, func(t *testing.T) {
			s, records, _, host := forceRestartFixture(t, 1)
			host.failure = cluster.MemberRestartError{Reason: why}
			if err := s.Reconcile(t.Context()); err != nil {
				t.Fatal(err)
			}
			got, _ := s.attempts.Get(t.Context(), records[0].ID)
			if len(host.starts) != 0 || got.ForceStop.Level != "exhausted" || got.ForceStop.Reason != why {
				t.Fatalf("unsafe restart: %+v %d", got.ForceStop, len(host.starts))
			}
		})
	}
}

func TestForceRestartLostHolderAndPendingDispatchNeverRepeat(t *testing.T) {
	s, records, sessions, host := forceRestartFixture(t, 1)
	r, err := s.attempts.RecordForceStopResult(t.Context(), records[0].ID, 1, true, "unavailable")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.attempts.BeginForceRestart(t.Context(), r.ID, 1, "cluster", "link-owner"); err != nil {
		t.Fatal(err)
	}
	sessions.failure = errors.New("no response")
	if err := s.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, _ := s.attempts.Get(t.Context(), r.ID)
	if len(host.starts) != 0 || got.ForceStop.Level != "exhausted" || got.ForceStop.Reason != "restart_status_lost" {
		t.Fatalf("reserved dispatch was replayed: %+v %v", got.ForceStop, host.starts)
	}
}

func TestForceRestartAwaitNeverAcceptsCommandCompletion(t *testing.T) {
	s, records, sessions, host := forceRestartFixture(t, 1)
	if err := s.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	host.status.State = "connected"
	if err := s.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	sessions.failure = &node.SessionError{Code: "stop_unproven", Message: "no process proof"}
	if err := s.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, _ := s.attempts.Get(t.Context(), records[0].ID)
	if !got.Unsettled || got.ForceStop.Level != "exhausted" || got.ForceStop.Reason != "stop_unproven" || len(host.starts) != 1 {
		t.Fatalf("unproved return confirmed: %+v", got)
	}
}

func TestForceRestartRejectsAnotherPlansSuccessfulStatus(t *testing.T) {
	s, records, _, host := forceRestartFixture(t, 1)
	if err := s.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	host.status.State = "connected"
	host.status.PlanID = "another-plan"
	if err := s.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, _ := s.attempts.Get(t.Context(), records[0].ID)
	if got.ForceStop.Level != "exhausted" || got.ForceStop.Reason != "restart_status_lost" {
		t.Fatalf("another plan's result advanced restart: %+v", got.ForceStop)
	}
}

func TestForceRestartLaterAttemptJoinsTheSameNodeOperation(t *testing.T) {
	s, records, _, host := forceRestartFixture(t, 2)
	for _, r := range records {
		if err := s.forceStop(t.Context(), r); err != nil {
			t.Fatal(err)
		}
	}
	one, _ := s.attempts.Get(t.Context(), records[0].ID)
	two, _ := s.attempts.Get(t.Context(), records[1].ID)
	if len(host.starts) != 1 || one.ForceStop.RestartID != two.ForceStop.RestartID {
		t.Fatalf("one machine got separate operations: starts=%d one=%+v two=%+v", len(host.starts), one.ForceStop, two.ForceStop)
	}
}
