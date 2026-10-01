package sshconnect

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestMemberRestartClaimsBeforeSSHAndDoesNotReplay(t *testing.T) {
	s, runner, _ := restartFixture(t)
	var claims atomic.Int32
	claim := func(context.Context, string, string) (bool, error) {
		claims.Add(1)
		if len(runner.restarts()) != 0 {
			t.Error("SSH preceded durable claim")
		}
		return true, nil
	}
	first, err := s.BeginMemberRestart(t.Context(), "node-1", "request", claim)
	if err != nil || first.PlanID == "" {
		t.Fatalf("start: %+v %v", first, err)
	}
	second, err := s.BeginMemberRestart(t.Context(), "node-1", "request", claim)
	if err != nil || second.PlanID != first.PlanID {
		t.Fatalf("replay created another plan: %+v %v", second, err)
	}
	waitUntil(t, "restart never settled", func() bool {
		st, _ := s.MemberRestartStatus(t.Context(), "node-1", "request", first.PlanID, "restart")
		return st.State == "connected"
	})
	if claims.Load() != 1 || len(runner.restarts()) != 1 {
		t.Fatalf("claims=%d SSH=%d", claims.Load(), len(runner.restarts()))
	}
	st, _ := s.MemberRestartStatus(t.Context(), "node-1", "request", "unrelated", "restart")
	if st.State != "lost" {
		t.Fatalf("unrelated plan accepted: %+v", st)
	}
}

func TestMemberRestartRefusedOrLostClaimNeverRunsSSH(t *testing.T) {
	for _, refused := range []bool{false, true} {
		t.Run(map[bool]string{false: "already-claimed", true: "write-refused"}[refused], func(t *testing.T) {
			s, runner, _ := restartFixture(t)
			_, err := s.BeginMemberRestart(t.Context(), "node-1", "request", func(context.Context, string, string) (bool, error) {
				if refused {
					return false, errors.New("replication unavailable")
				}
				return false, nil
			})
			if err == nil {
				t.Fatal("unaccepted claim reported a started restart")
			}
			if len(runner.restarts()) != 0 {
				t.Fatal("SSH started without unique committed claim")
			}
		})
	}
}

func TestMemberRestartJoinsMachineUpgradeWithoutRestarting(t *testing.T) {
	s, runner, _ := restartFixture(t)
	s.mu.Lock()
	s.upgrades = map[string]string{"node-1": "upgrade-plan"}
	stored := &storedPlan{plan: InstallPlan{ID: "upgrade-plan", Request: InstallRequest{Name: "node-1"}}, running: true, result: InstallResult{PlanID: "upgrade-plan", NodeID: "node-1", Status: "installing", Phase: PhaseConnectivity}}
	s.plans["upgrade-plan"] = stored
	s.mu.Unlock()
	var claims atomic.Int32
	st, err := s.BeginMemberRestart(t.Context(), "node-1", "request", func(_ context.Context, plan, kind string) (bool, error) {
		claims.Add(1)
		if plan != "upgrade-plan" || kind != "upgrade" {
			t.Errorf("joined %s %s", plan, kind)
		}
		return true, nil
	})
	if err != nil || st.Kind != "upgrade" || st.PlanID != "upgrade-plan" {
		t.Fatalf("busy upgrade not joined: %+v %v", st, err)
	}
	s.mu.Lock()
	stored.running = false
	stored.result.Status = "connected"
	stored.result.Connected = true
	s.mu.Unlock()
	st, err = s.MemberRestartStatus(t.Context(), "node-1", "request", "upgrade-plan", "upgrade")
	if err != nil || st.State != "connected" || claims.Load() != 1 || len(runner.restarts()) != 0 {
		t.Fatalf("upgrade turned into restart: %+v %v", st, err)
	}
}

func TestMemberRestartKeepsMachineSlotUntilClaimReturns(t *testing.T) {
	s, runner, _ := restartFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := s.BeginMemberRestart(t.Context(), "node-1", "one", func(context.Context, string, string) (bool, error) { close(entered); <-release; return true, nil })
		done <- err
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("request returned before claim: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("claim not entered")
	}
	s.mu.Lock()
	running := s.running(s.restarts, "node-1")
	s.mu.Unlock()
	if !running {
		t.Error("machine slot not held before claim")
	}
	if len(runner.restarts()) != 0 {
		t.Error("SSH before claim completed")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
