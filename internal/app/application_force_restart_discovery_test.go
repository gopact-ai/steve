package app

import (
	"context"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/cluster"
)

type discoveryRestartHost struct {
	*forceRestartHost
	find func(context.Context, string, string) (cluster.RestartTarget, error)
}

func (h discoveryRestartHost) Find(ctx context.Context, node, by string) (cluster.RestartTarget, error) {
	return h.find(ctx, node, by)
}

func TestForceRestartDiscoveryHasADurableDeadlineWithoutAnOperation(t *testing.T) {
	s, records, _, host := forceRestartFixture(t, 1)
	r, err := s.attempts.RecordForceStopResult(t.Context(), records[0].ID, 1, true, "unavailable")
	if err != nil {
		t.Fatal(err)
	}
	clock := r.ForceStop.LevelSince
	s.now = func() time.Time { return clock }
	var queries int
	for pass := range 2 {
		ctx, cancel := context.WithCancel(t.Context())
		s.restarts = discoveryRestartHost{host, func(ctx context.Context, _, _ string) (cluster.RestartTarget, error) {
			queries++
			cancel()
			return cluster.RestartTarget{}, ctx.Err()
		}}
		clock = r.ForceStop.LevelSince.Add(time.Duration(pass+1) * time.Minute)
		if err := s.restartForceStop(ctx, r); err == nil {
			t.Fatal("cancelled discovery was reported as complete")
		}
		cancel()
	}
	clock = r.ForceStop.LevelSince.Add(7 * time.Minute)
	s.restarts = discoveryRestartHost{host, func(context.Context, string, string) (cluster.RestartTarget, error) {
		queries++
		return cluster.RestartTarget{ClusterID: "cluster", Holder: "link-owner"}, nil
	}}
	if err := s.restartForceStop(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	got, err := s.attempts.Get(t.Context(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if queries != 2 || len(host.starts) != 0 || got.ForceStop.Level != "exhausted" || got.ForceStop.Reason != "restart_timeout" {
		t.Fatalf("discovery outlived its durable phase: queries=%d starts=%d state=%+v", queries, len(host.starts), got.ForceStop)
	}
	if _, found, err := s.attempts.ForceRestart(t.Context(), r.Node); err != nil || found {
		t.Fatalf("expired discovery reserved a restart: found=%v err=%v", found, err)
	}
}

func TestForceRestartHolderFoundAfterDeadlineDoesNotGetANewBudget(t *testing.T) {
	s, records, _, host := forceRestartFixture(t, 1)
	r, err := s.attempts.RecordForceStopResult(t.Context(), records[0].ID, 1, true, "unavailable")
	if err != nil {
		t.Fatal(err)
	}
	clock := r.ForceStop.LevelSince.Add(7*time.Minute - time.Millisecond)
	s.now = func() time.Time { return clock }
	s.restarts = discoveryRestartHost{host, func(context.Context, string, string) (cluster.RestartTarget, error) {
		clock = r.ForceStop.LevelSince.Add(7 * time.Minute)
		return cluster.RestartTarget{ClusterID: "cluster", Holder: "link-owner"}, nil
	}}
	if err := s.restartForceStop(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	got, err := s.attempts.Get(t.Context(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(host.starts) != 0 || got.ForceStop.Level != "exhausted" || got.ForceStop.Reason != "restart_timeout" {
		t.Fatalf("late discovery restarted the machine: starts=%d state=%+v", len(host.starts), got.ForceStop)
	}
}

func TestForceRestartEarlierAttemptDeadlineDoesNotExpireSharedOperation(t *testing.T) {
	s, records, _, host := forceRestartFixture(t, 2)
	for _, r := range records {
		if err := s.forceStop(t.Context(), r); err != nil {
			t.Fatal(err)
		}
	}
	one, err := s.attempts.Get(t.Context(), records[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	two, err := s.attempts.Get(t.Context(), records[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	op, found, err := s.attempts.ForceRestart(t.Context(), one.Node)
	if err != nil || !found {
		t.Fatalf("missing shared operation: %v %v", found, err)
	}
	// The first discovery began before this shared operation. Its snapshot
	// expires a minute sooner; that cannot expire the other attempt's plan.
	force := *one.ForceStop
	force.LevelSince = op.RequestedAt.Add(-time.Minute)
	one.ForceStop = &force
	clock := op.RequestedAt.Add(6 * time.Minute)
	s.now = func() time.Time { return clock }
	if err := s.pollForceRestart(t.Context(), one); err != nil {
		t.Fatal(err)
	}
	if err := s.pollForceRestart(t.Context(), two); err != nil {
		t.Fatal(err)
	}
	exhausted, _ := s.attempts.Get(t.Context(), one.ID)
	waiting, _ := s.attempts.Get(t.Context(), two.ID)
	current, _, err := s.attempts.ForceRestart(t.Context(), one.Node)
	if err != nil || exhausted.ForceStop.Level != "exhausted" || waiting.ForceStop.Level != "restart" || !current.FinishedAt.IsZero() || len(host.starts) != 1 {
		t.Fatalf("one deadline ended shared work: first=%+v second=%+v operation=%+v starts=%d err=%v", exhausted.ForceStop, waiting.ForceStop, current, len(host.starts), err)
	}
	clock = op.RequestedAt.Add(7 * time.Minute)
	if err := s.pollForceRestart(t.Context(), two); err != nil {
		t.Fatal(err)
	}
	waiting, _ = s.attempts.Get(t.Context(), two.ID)
	current, _, err = s.attempts.ForceRestart(t.Context(), one.Node)
	if err != nil || waiting.ForceStop.Level != "exhausted" || current.Outcome != "timeout" || current.FinishedAt.IsZero() || len(host.starts) != 1 {
		t.Fatalf("shared deadline did not end all work: state=%+v operation=%+v starts=%d err=%v", waiting.ForceStop, current, len(host.starts), err)
	}
}
