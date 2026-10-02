package cluster

import (
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/coordination"
)

func TestDemotedReplicaKeepsFollowingWithoutBusinessAuthority(t *testing.T) {
	nodes := testNodesWith(t, 3, steadyTiming)
	first := openNode(t, nodes[0])
	old := ready(t, first)
	second := joinNode(t, first, nodes[1], true, false)
	joinNode(t, first, nodes[2], true, false)
	if _, err := first.Transfer(t.Context(), coordination.TransferRequest{ID: "move-before-vote-removal", Actor: "owner", ExpectedEpoch: old.Assignment.Epoch, TargetNodeID: "node-2"}); err != nil {
		t.Fatal(err)
	}
	active := ready(t, second)
	if !first.Status().IsLeader {
		t.Fatal("test requires the old coordinator to remain consensus leader")
	}
	state, err := second.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.SetVoting(t.Context(), coordination.VotingRequest{ID: "demote-old-coordinator", Actor: "owner", ExpectedRevision: state.Revision, NodeID: "node-1", Voting: false}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 8*time.Second, "healthy nonvoting replica", func() bool {
		s := first.Status()
		return s.Healthy && !s.Ready && s.IsActiveReplica("node-1") && s.Voters["node-1"] == ""
	})
	if err := old.Ledger.Document("old-generation-after-demotion").Save([]byte("denied")); err == nil {
		t.Fatal("demotion revived the old generation's write authority")
	}
	if err := active.Ledger.Document("after-demotion").Save([]byte("replicated")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 8*time.Second, "demoted node still receives replication", func() bool {
		raw, found, err := first.Ledger().Document("after-demotion").Load()
		return err == nil && found && string(raw) == "replicated"
	})
	if _, err := second.Remove(t.Context(), coordination.RemoveRequest{ID: "remove-demoted-member", Actor: "owner", NodeID: "node-1"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 8*time.Second, "removed member is not active", func() bool {
		return !first.Status().IsActiveReplica("node-1") && !first.Status().Ready
	})
	if err := old.Ledger.Document("old-generation-after-removal").Save([]byte("denied")); err == nil {
		t.Fatal("removed node retained business write authority")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
}
