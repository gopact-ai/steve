package coordination

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/stableport"
	"github.com/hashicorp/raft"
)

type testCluster struct {
	t       *testing.T
	mu      sync.RWMutex
	nodes   map[string]*Service
	configs map[string]Config
}

func newTestCluster(t *testing.T, count int, applications ...func(string, string) Application) *testCluster {
	t.Helper()
	c := &testCluster{t: t, nodes: map[string]*Service{}, configs: map[string]Config{}}
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("node-%d", i+1)
		cfg := raft.DefaultConfig()
		cfg.HeartbeatTimeout = 180 * time.Millisecond
		cfg.ElectionTimeout = 180 * time.Millisecond
		cfg.LeaderLeaseTimeout = 90 * time.Millisecond
		cfg.CommitTimeout = 10 * time.Millisecond
		cfg.SnapshotInterval = 20 * time.Second
		listener, err := stableport.Listen(net.Listen, "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := listener.Addr().String()
		listener.Close()
		config := Config{ClusterID: "test-cluster", NodeID: id, FailureDomain: "test-domain-" + id, StorageLevel: "restricted", DataDir: t.TempDir(), BindAddress: addr, Bootstrap: i == 0, RaftConfig: cfg, LogOutput: io.Discard, ApplyTimeout: time.Second, FailoverTimeout: 300 * time.Millisecond, ProbeInterval: 40 * time.Millisecond, Probe: c.probe}
		if len(applications) > 0 {
			config.Application = applications[0](id, config.DataDir)
		}
		c.configs[id] = config
		n, err := Open(config)
		if err != nil {
			t.Fatal(err)
		}
		c.mu.Lock()
		c.nodes[id] = n
		c.mu.Unlock()
	}
	t.Cleanup(func() {
		c.mu.RLock()
		defer c.mu.RUnlock()
		for _, n := range c.nodes {
			n.Close()
		}
	})
	leader := c.leader()
	for i := 1; i < count; i++ {
		id := fmt.Sprintf("node-%d", i+1)
		_, err := leader.Join(context.Background(), JoinRequest{ID: "join-" + id, Actor: "user", Member: Member{NodeID: id, Address: c.nodes[id].Status().Address, Voting: true}})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(applications) > 0 {
		if _, err := leader.BeginWriter(t.Context(), WriterRequest{ID: "initial-writer", CallerNodeID: "node-1", CoordinatorEpoch: 1}); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func (c *testCluster) probe(ctx context.Context, m Member) (Progress, error) {
	c.mu.RLock()
	n := c.nodes[m.NodeID]
	c.mu.RUnlock()
	if n == nil || !n.Status().Healthy {
		return Progress{}, ErrUnavailable
	}
	return n.Status().Progress(), nil
}

func (c *testCluster) leader() *Service {
	c.t.Helper()
	var found *Service
	eventually(c.t, 5*time.Second, func() bool {
		c.mu.RLock()
		defer c.mu.RUnlock()
		for _, n := range c.nodes {
			if n.Status().IsLeader && n.Status().Coordinator.Epoch > 0 {
				found = n
				return true
			}
		}
		return false
	})
	return found
}

func (c *testCluster) stop(id string) {
	c.t.Helper()
	c.mu.Lock()
	n := c.nodes[id]
	delete(c.nodes, id)
	c.mu.Unlock()
	if err := n.Close(); err != nil {
		c.t.Fatal(err)
	}
}

func eventually(t *testing.T, d time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func TestSingleNodeStartsUsableAndKeepsStableIdentity(t *testing.T) {
	c := newTestCluster(t, 1)
	n := c.leader()
	state, err := n.ReadState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.Coordinator != (Assignment{NodeID: "node-1", Epoch: 1}) || state.AutoFailover || state.Members["node-1"].AutoEligible || len(state.Voters) != 1 {
		t.Fatalf("unexpected initial state: %+v", state)
	}
	_, err = n.SetAutoFailover(context.Background(), PolicyRequest{ID: "enable", Actor: "user", ExpectedRevision: state.Revision, Enabled: true})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("single node enabled failover: %v", err)
	}
	c.stop("node-1")
	bad := c.configs["node-1"]
	bad.NodeID = "other"
	if v, e := Open(bad); e == nil {
		v.Close()
		t.Fatal("changed durable identity accepted")
	}
	n, err = Open(c.configs["node-1"])
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.nodes["node-1"] = n
	c.mu.Unlock()
	restored := c.leader().Status()
	if restored.ClusterID != state.ClusterID || restored.Coordinator != state.Coordinator {
		t.Fatalf("restart lost identity: %+v", restored)
	}
}

// A quorum read commits a barrier that the state machine never sees: Raft's
// applied index covers it, so a replica that has applied everything it
// committed reports the two indexes equal, while the state's own applied
// index stays at the last command or membership change. The check repeats
// so that it holds for every read rather than for one lucky interleaving.
//
// The barrier is committed before the read returns, but Raft records an
// entry as applied only after handing it to the state machine, which may
// answer the barrier first. The test gives the applied index up to the
// ApplyTimeout the runtime allows a committed entry to wait to catch up.
func TestLogProgressCoversEntriesTheStateMachineNeverSees(t *testing.T) {
	c := newTestCluster(t, 1)
	n := c.leader()
	applyTimeout := n.config.ApplyTimeout
	for read := 0; read < 200; read++ {
		// The read's barrier is the first entry after the ones the log
		// holds now.
		before := n.LastIndex()
		state, err := n.ReadState(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		progress := n.LogProgress()
		if progress.Committed <= before {
			t.Fatalf("quorum read %d returned before the single node committed its barrier: committed %d, and the log held %d entries before the read", read, progress.Committed, before)
		}
		deadline := time.Now().Add(applyTimeout)
		for progress.Applied < progress.Committed && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
			progress = n.LogProgress()
		}
		if progress.Applied != progress.Committed {
			t.Fatalf("%s after quorum read %d the single node has committed %d and applied %d of %d entries", applyTimeout, read, progress.Committed, progress.Applied, n.LastIndex())
		}
		if state.AppliedIndex >= progress.Applied {
			t.Fatalf("after quorum read %d the state applied index %d reaches the barrier at %d: the state machine now sees barriers", read, state.AppliedIndex, progress.Applied)
		}
	}
}

func TestTwoNodesOnlyManualTransferWithEpochAndDeduplication(t *testing.T) {
	c := newTestCluster(t, 2)
	n := c.leader()
	state, err := n.ReadState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = n.SetAutoFailover(context.Background(), PolicyRequest{ID: "enable", Actor: "user", ExpectedRevision: state.Revision, Enabled: true})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("two nodes enabled failover: %v", err)
	}
	req := TransferRequest{ID: "manual", Actor: "user", ExpectedEpoch: 1, TargetNodeID: "node-2", Reason: "user request"}
	result, err := n.Transfer(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Coordinator != (Assignment{NodeID: "node-2", Epoch: 2}) {
		t.Fatalf("assignment: %+v", result)
	}
	duplicate, err := n.Transfer(context.Background(), req)
	if err != nil || !reflect.DeepEqual(duplicate, result) {
		t.Fatalf("retry: %+v, %v", duplicate, err)
	}
	_, err = n.Transfer(context.Background(), TransferRequest{ID: "stale", Actor: "user", ExpectedEpoch: 1, TargetNodeID: "node-1"})
	if !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("old epoch accepted: %v", err)
	}
	req.TargetNodeID = "node-1"
	if _, err = n.Transfer(context.Background(), req); !errors.Is(err, ErrCommandConflict) {
		t.Fatalf("reused command ID accepted: %v", err)
	}
	c.stop("node-2")
	_, err = n.Transfer(context.Background(), TransferRequest{ID: "without-quorum", Actor: "user", ExpectedEpoch: 2, TargetNodeID: "node-1"})
	if err == nil {
		t.Fatal("minority committed transfer")
	}
	if n.Status().Coordinator != result.Coordinator || len(n.Status().Voters) != 2 {
		t.Fatal("loss of peer changed assignment or shrank voters")
	}
}

func TestConsensusElectionDoesNotEnableBusinessFailover(t *testing.T) {
	c := newTestCluster(t, 3)
	c.stop("node-1")
	n := c.leader()
	state, err := n.ReadState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.Coordinator.NodeID != "node-1" || state.AutoFailover {
		t.Fatalf("internal election changed coordinator: %+v", state)
	}
	time.Sleep(450 * time.Millisecond)
	if n.Status().Coordinator.NodeID != "node-1" {
		t.Fatal("disabled automatic failover occurred")
	}
	result, err := n.Transfer(context.Background(), TransferRequest{ID: "manual-after-failure", Actor: "user", ExpectedEpoch: 1, TargetNodeID: n.Status().NodeID})
	if err != nil {
		t.Fatal(err)
	}
	if result.Coordinator.Epoch != 2 {
		t.Fatal("manual transfer did not fence old coordinator")
	}
}

func TestAutomaticFailoverUsesQuorumAndEligibleCaughtUpNode(t *testing.T) {
	c := newTestCluster(t, 3)
	n := c.leader()
	state, err := n.ReadState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = n.SetEligibility(context.Background(), EligibilityRequest{ID: "eligible-1", Actor: "user", ExpectedRevision: state.Revision, NodeID: "node-1", Eligible: true}); err != nil {
		t.Fatal(err)
	}
	state, err = n.ReadState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = n.SetEligibility(context.Background(), EligibilityRequest{ID: "eligible-2", Actor: "user", ExpectedRevision: state.Revision, NodeID: "node-2", Eligible: true})
	if err != nil {
		t.Fatal(err)
	}
	state, err = n.ReadState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = n.SetAutoFailover(context.Background(), PolicyRequest{ID: "enable", Actor: "user", ExpectedRevision: state.Revision, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, time.Second, func() bool { return c.nodes["node-2"].Status().AutoFailover && c.nodes["node-3"].Status().AutoFailover })
	c.stop("node-1")
	n = c.leader()
	eventually(t, 4*time.Second, func() bool { return n.Status().Coordinator == (Assignment{NodeID: "node-2", Epoch: 2}) })
	state, err = n.ReadState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Voters) != 3 {
		t.Fatal("failed member was removed from quorum")
	}
	last := state.Audit[len(state.Audit)-1]
	if last.Kind != "coordinator_transferred" || last.From != "node-1" || last.To != "node-2" || last.Actor != "system" {
		t.Fatalf("missing automatic transfer audit: %+v", last)
	}
}

func TestRenameMemberReplicatesDisplayName(t *testing.T) {
	c := newTestCluster(t, 3)
	n := c.leader()
	state, err := n.ReadState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.Rename(context.Background(), RenameRequest{ID: "rename-1", Actor: "user", ExpectedRevision: state.Revision, NodeID: "node-2", Name: "  GPU 工作站  "}); err != nil {
		t.Fatal(err)
	}
	renamed, err := n.ReadState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Members["node-2"].Name != "GPU 工作站" {
		t.Fatalf("rename did not trim and store the display name: %+v", renamed.Members["node-2"])
	}
	if renamed.Revision != state.Revision+1 {
		t.Fatalf("rename must advance the membership revision: %d -> %d", state.Revision, renamed.Revision)
	}
	last := renamed.Audit[len(renamed.Audit)-1]
	if last.Kind != "member_renamed" || last.To != "node-2" || !strings.HasSuffix(last.Reason, "-> GPU 工作站") {
		t.Fatalf("rename audit record: %+v", last)
	}
	// The same command ID replays as the recorded receipt without a second revision bump.
	if _, err := n.Rename(context.Background(), RenameRequest{ID: "rename-1", Actor: "user", ExpectedRevision: state.Revision, NodeID: "node-2", Name: "  GPU 工作站  "}); err != nil {
		t.Fatal(err)
	}
	eventually(t, time.Second, func() bool { return c.nodes["node-3"].Status().State.Members["node-2"].Name == "GPU 工作站" })
	if _, err := n.Rename(context.Background(), RenameRequest{ID: "rename-stale", Actor: "user", ExpectedRevision: state.Revision, NodeID: "node-2", Name: "stale"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale revision must be rejected as a conflict: %v", err)
	}
	for name, request := range map[string]RenameRequest{
		"unknown member": {ID: "rename-unknown", Actor: "user", ExpectedRevision: renamed.Revision, NodeID: "node-9", Name: "ghost"},
		"blank name":     {ID: "rename-blank", Actor: "user", ExpectedRevision: renamed.Revision, NodeID: "node-2", Name: " \t"},
		"control chars":  {ID: "rename-control", Actor: "user", ExpectedRevision: renamed.Revision, NodeID: "node-2", Name: "bad\nname"},
		"too long":       {ID: "rename-long", Actor: "user", ExpectedRevision: renamed.Revision, NodeID: "node-2", Name: strings.Repeat("长", 65)},
	} {
		if _, err := n.Rename(context.Background(), request); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s must be rejected as invalid: %v", name, err)
		}
	}
	final, err := n.ReadState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if final.Revision != renamed.Revision || final.Members["node-2"].Name != "GPU 工作站" {
		t.Fatalf("rejected renames must leave state untouched: %+v", final.Members["node-2"])
	}
}
