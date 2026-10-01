package sshconnect

import (
	"context"
	"sync/atomic"
	"testing"
)

func TestMemberRestartJoinCannotBecomeAFreshRestart(t *testing.T) {
	for _, mode := range []string{"missing", "replaced", "wrong-kind"} {
		t.Run(mode, func(t *testing.T) {
			s, runner, _ := restartFixture(t)
			choice := MemberRestartChoice{PlanID: "observed", Kind: "restart"}
			s.mu.Lock()
			if mode != "missing" {
				stored, failure := s.claimRestart(s.text, "node-1", false)
				if failure != nil {
					s.mu.Unlock()
					t.Fatal(failure)
				}
				if mode == "wrong-kind" {
					choice.PlanID, choice.Kind = stored.plan.ID, "upgrade"
				}
			}
			s.mu.Unlock()
			var claims atomic.Int32
			_, err := s.BeginMemberRestart(t.Context(), "node-1", "join-only", "cluster", func(context.Context, string, string) (bool, error) { claims.Add(1); return true, nil }, func(context.Context, string, string) error { return nil }, choice)
			if err == nil || claims.Load() != 0 || len(runner.restarts()) != 0 {
				t.Fatalf("lost selected plan started new work: err=%v claims=%d scripts=%d", err, claims.Load(), len(runner.restarts()))
			}
		})
	}
}

func TestMemberRestartFreshChoiceDoesNotJoinUnobservedActivity(t *testing.T) {
	s, runner, _ := restartFixture(t)
	s.mu.Lock()
	_, failure := s.claimRestart(s.text, "node-1", false)
	s.mu.Unlock()
	if failure != nil {
		t.Fatal(failure)
	}
	var claims atomic.Int32
	_, err := s.BeginMemberRestart(t.Context(), "node-1", "stale-idle", "cluster", func(context.Context, string, string) (bool, error) { claims.Add(1); return true, nil }, func(context.Context, string, string) error { return nil }, MemberRestartChoice{})
	if err == nil || claims.Load() != 0 || len(runner.restarts()) != 0 {
		t.Fatalf("unobserved activity replaced a fresh choice: err=%v claims=%d scripts=%d", err, claims.Load(), len(runner.restarts()))
	}
}
