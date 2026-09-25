package coordination

import (
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/raft"
)

type resolvedFuture struct{ err error }

func (f resolvedFuture) Error() error { return f.err }

// While its own leadership transfer runs, a leader rejects applies, barriers,
// configuration reads and changes, restores and further transfers. The
// rejection ends with the transfer, so callers must see it as retryable, not
// as a bad request.
func TestWaitReportsTransientRaftRejectionsAsUnavailable(t *testing.T) {
	s := &Service{ctx: t.Context(), fsm: newMachine("test-cluster", nil)}
	for _, cause := range []error{raft.ErrLeadershipTransferInProgress, raft.ErrLeadershipLost, raft.ErrRaftShutdown, raft.ErrEnqueueTimeout} {
		if err := s.wait(t.Context(), resolvedFuture{cause}); !errors.Is(err, ErrUnavailable) {
			t.Errorf("%v was reported as %v", cause, err)
		}
	}
}

// Removing the consensus leader first hands leadership to a caught-up voter.
// Raft reports a transfer that did not finish within the election timeout
// without a sentinel error, and the target may still take over afterwards,
// so the caller must be told to retry rather than that the request is bad.
func TestRemoveLeaderReportsUnfinishedLeadershipTransferAsUnavailable(t *testing.T) {
	c := newTestCluster(t, 3)
	leader := c.leader()
	var others []string
	for id := range leader.Status().Voters {
		if id != leader.config.NodeID {
			others = append(others, id)
		}
	}
	sort.Strings(others)
	if len(others) != 2 {
		t.Fatalf("voters besides the leader: %v", others)
	}
	// Remove tries targets in ID order; the first one receives the transfer.
	target, coordinator := others[0], others[1]
	eventually(t, 5*time.Second, func() bool {
		_, err := leader.Transfer(t.Context(), TransferRequest{ID: "move-before-remove", Actor: "owner", ExpectedEpoch: 1, TargetNodeID: coordinator})
		if errors.Is(err, ErrNotReady) {
			return false
		}
		if err != nil {
			t.Fatal(err)
		}
		return true
	})
	applied := leader.Status().AppliedIndex
	eventually(t, 5*time.Second, func() bool { return c.nodes[target].Status().AppliedIndex >= applied })
	// The target keeps answering heartbeats and progress probes but never
	// handles the TimeoutNow request.
	if err := newRaftLoopPause(t, c.nodes[target]).pause(); err != nil {
		t.Fatal(err)
	}
	_, err := leader.Remove(t.Context(), RemoveRequest{ID: "remove-leader", Actor: "owner", NodeID: leader.config.NodeID})
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "consensus leadership transfer to "+target) {
		t.Fatalf("unfinished leadership transfer returned %v", err)
	}
}
