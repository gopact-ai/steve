package coordination

import (
	"context"
	"sync/atomic"
	"testing"
)

func TestTransferRetriesTemporaryProbeFailureWithinItsDeadline(t *testing.T) {
	cluster := newTestCluster(t, 2)
	node := cluster.leader()
	probe := node.config.Probe
	var failures atomic.Int64
	node.config.Probe = func(ctx context.Context, member Member) (Progress, error) {
		if failures.Add(1) == 1 {
			return Progress{}, ErrUnavailable
		}
		return probe(ctx, member)
	}
	result, err := node.Transfer(t.Context(), TransferRequest{ID: "transfer-after-probe-retry", Actor: "owner", ExpectedEpoch: 1, TargetNodeID: "node-2"})
	if err != nil || result.Coordinator.NodeID != "node-2" || failures.Load() < 2 {
		t.Fatalf("temporary progress failure aborted a healthy transfer: %+v %v", result, err)
	}
}
