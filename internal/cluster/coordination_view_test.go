package cluster

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/coordination"
)

func TestCoordinationViewAllowsSlowReachablePeersWithoutRelaxingReadiness(t *testing.T) {
	state := coordination.State{AppliedIndex: 20, AppVersion: 3, Members: map[string]coordination.Member{}, Replicas: map[string]string{}}
	for _, id := range []string{"a-ready", "b-lagging", "c-pending"} {
		state.Members[id] = coordination.Member{NodeID: id, Address: id}
		state.Replicas[id] = id
	}
	state.PendingJoins = map[string]bool{"c-pending": true}
	probe := func(ctx context.Context, m coordination.Member) (coordination.Progress, error) {
		select {
		case <-time.After(1100 * time.Millisecond):
		case <-ctx.Done():
			return coordination.Progress{}, ctx.Err()
		}
		p := coordination.Progress{AppliedIndex: 20, AppVersion: 3}
		if m.NodeID == "b-lagging" {
			p.AppVersion--
		}
		return p, nil
	}
	start := time.Now()
	nodes := coordinationNodes(t.Context(), state, "local", Status{}, probe)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("peer latency accumulated sequentially: %s", elapsed)
	}
	for _, node := range nodes {
		if !node.Online {
			t.Errorf("%s was reachable within the peer transport budget but reported offline", node.ID)
		}
		if node.Ready != (node.ID == "a-ready") {
			t.Errorf("%s readiness bypassed application or membership fence: %+v", node.ID, node)
		}
	}
}

func TestCoordinationViewProbesAreBoundedAndCancel(t *testing.T) {
	state := coordination.State{Members: map[string]coordination.Member{}}
	for _, id := range []string{"a", "b", "c", "d", "e", "f"} {
		state.Members[id] = coordination.Member{NodeID: id, Address: id}
	}
	var active, peak atomic.Int32
	probe := func(ctx context.Context, _ coordination.Member) (coordination.Progress, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		<-ctx.Done()
		return coordination.Progress{}, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	nodes := coordinationNodes(ctx, state, "local", Status{}, probe)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("cancelled probes did not join promptly: %s", elapsed)
	}
	if peak.Load() < 2 || peak.Load() > 4 || active.Load() != 0 {
		t.Fatalf("unexpected probe concurrency: peak=%d active=%d", peak.Load(), active.Load())
	}
	for i, node := range nodes {
		if node.Online || (i > 0 && nodes[i-1].ID > node.ID) {
			t.Fatalf("cancelled view lost ordering or reported online: %+v", nodes)
		}
	}
}
