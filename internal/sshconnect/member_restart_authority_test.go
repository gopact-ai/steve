package sshconnect

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type blockedRestartProbe struct {
	*restartRunner
	entered, release chan struct{}
	once             sync.Once
}

func (r *blockedRestartProbe) Bind(_ context.Context, _ string, args []string) (Connection, error) {
	return &fixtureConnection{runner: r, args: append([]string{}, args...)}, nil
}
func (r *blockedRestartProbe) Run(ctx context.Context, args []string, input string) (Output, error) {
	if strings.Contains(input, "STEVE_CHECK") {
		r.once.Do(func() { close(r.entered) })
		select {
		case <-r.release:
		case <-ctx.Done():
			return Output{}, ctx.Err()
		}
	}
	return r.restartRunner.Run(ctx, args, input)
}

func TestMemberRestartRevalidatesAfterPreflightBeforeTheScript(t *testing.T) {
	for _, reason := range []string{"owner changed", "coordinator changed", "operation changed"} {
		t.Run(reason, func(t *testing.T) {
			s, runner, _ := restartFixture(t)
			probe := &blockedRestartProbe{restartRunner: runner, entered: make(chan struct{}), release: make(chan struct{})}
			s.runner = probe
			var claimed, verified atomic.Int32
			var revoked atomic.Bool
			op, err := s.BeginMemberRestart(t.Context(), "node-1", "request", func(context.Context, string, string) (bool, error) { claimed.Add(1); return true, nil }, func(_ context.Context, plan, kind string) error {
				verified.Add(1)
				if plan == "" || kind != "restart" {
					return errors.New("wrong local slot")
				}
				if revoked.Load() {
					return errors.New(reason)
				}
				return nil
			})
			if err != nil {
				close(probe.release)
				t.Fatal(err)
			}
			select {
			case <-probe.entered:
			case <-time.After(2 * time.Second):
				close(probe.release)
				t.Fatal("preflight not entered")
			}
			revoked.Store(true)
			close(probe.release)
			waitUntil(t, "restart did not settle", func() bool {
				st, _ := s.MemberRestartStatus(t.Context(), "node-1", "request", op.PlanID, "restart")
				return st.State == "failed" || st.State == "connected"
			})
			if claimed.Load() != 1 || verified.Load() != 1 || len(runner.restarts()) != 0 {
				t.Fatalf("revoked restart reached script: claim=%d verify=%d scripts=%d", claimed.Load(), verified.Load(), len(runner.restarts()))
			}
		})
	}
}
