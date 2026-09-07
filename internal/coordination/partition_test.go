package coordination

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/raft"
)

// partitionNetwork closes real TCP connections and refuses new connections
// across a partition. The application progress probe follows the same links.
type partitionNetwork struct {
	mu        sync.Mutex
	addresses map[string]string
	isolated  string
	links     []tcpLink
}

type tcpLink struct {
	from, to   string
	connection net.Conn
}
type partitionStream struct {
	net.Listener
	node    string
	network *partitionNetwork
}

func (n *partitionNetwork) blocked(from, to string) bool {
	return n.isolated != "" && (from == n.isolated) != (to == n.isolated)
}

func (s *partitionStream) Dial(address raft.ServerAddress, timeout time.Duration) (net.Conn, error) {
	s.network.mu.Lock()
	defer s.network.mu.Unlock()
	to := s.network.addresses[string(address)]
	if s.network.blocked(s.node, to) {
		return nil, errors.New("link partitioned")
	}
	conn, err := net.DialTimeout("tcp", string(address), timeout)
	if err != nil {
		return nil, err
	}
	s.network.links = append(s.network.links, tcpLink{from: s.node, to: to, connection: conn})
	return conn, nil
}

func (n *partitionNetwork) isolate(node string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.isolated = node
	for _, link := range n.links {
		if n.blocked(link.from, link.to) {
			link.connection.Close()
		}
	}
}

func newPartitionCluster(t *testing.T) (*testCluster, *partitionNetwork) {
	t.Helper()
	c := &testCluster{t: t, nodes: map[string]*Service{}, configs: map[string]Config{}}
	network := &partitionNetwork{addresses: map[string]string{}}
	for i := 1; i <= 3; i++ {
		id := fmt.Sprintf("node-%d", i)
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		network.mu.Lock()
		network.addresses[listener.Addr().String()] = id
		network.mu.Unlock()
		cfg := raft.DefaultConfig()
		cfg.HeartbeatTimeout = 180 * time.Millisecond
		cfg.ElectionTimeout = 180 * time.Millisecond
		cfg.LeaderLeaseTimeout = 90 * time.Millisecond
		cfg.CommitTimeout = 10 * time.Millisecond
		dir := t.TempDir()
		config := Config{ClusterID: "partition-test", NodeID: id, FailureDomain: "test-domain-" + id, StorageLevel: "restricted", DataDir: dir, Bootstrap: i == 1, Application: openCounter(t, dir), RaftConfig: cfg, StreamLayer: &partitionStream{Listener: listener, node: id, network: network}, ApplyTimeout: time.Second, FailoverTimeout: 300 * time.Millisecond, ProbeInterval: 40 * time.Millisecond, LogOutput: io.Discard, Probe: func(ctx context.Context, m Member) (Progress, error) {
			network.mu.Lock()
			blocked := network.blocked(id, m.NodeID)
			network.mu.Unlock()
			if blocked {
				return Progress{}, ErrUnavailable
			}
			return c.probe(ctx, m)
		}}
		node, err := Open(config)
		if err != nil {
			t.Fatal(err)
		}
		c.mu.Lock()
		c.nodes[id] = node
		c.mu.Unlock()
		c.configs[id] = config
	}
	t.Cleanup(func() {
		for _, node := range c.nodes {
			node.Close()
		}
	})
	leader := c.leader()
	for _, id := range []string{"node-2", "node-3"} {
		_, err := leader.Join(context.Background(), JoinRequest{ID: "join-" + id, Actor: "user", Member: Member{NodeID: id, Address: c.nodes[id].Status().Address, AutoEligible: true}})
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := leader.BeginWriter(t.Context(), WriterRequest{ID: "initial-writer", CallerNodeID: "node-1", CoordinatorEpoch: 1}); err != nil {
		t.Fatal(err)
	}
	return c, network
}

func TestNetworkPartitionFencesOldCoordinatorAndHealsWithoutSplitBrain(t *testing.T) {
	c, network := newPartitionCluster(t)
	original := c.leader()
	_, err := original.ApplyApp(context.Background(), AppCommand{WriterGeneration: 1, ID: "before-partition", CallerNodeID: "node-1", CoordinatorEpoch: 1, Payload: []byte("1")})
	if err != nil {
		t.Fatal(err)
	}
	state, err := original.ReadState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = original.SetAutoFailover(context.Background(), PolicyRequest{ID: "enable", Actor: "user", ExpectedRevision: state.Revision, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, time.Second, func() bool { return c.nodes["node-2"].Status().AutoFailover && c.nodes["node-3"].Status().AutoFailover })
	network.isolate("node-1")
	var majority *Service
	eventually(t, 5*time.Second, func() bool {
		for _, id := range []string{"node-2", "node-3"} {
			candidate := c.nodes[id]
			if candidate.Status().IsLeader && candidate.Status().Coordinator.Epoch == 2 {
				majority = candidate
				return true
			}
		}
		return false
	})
	_, err = original.ApplyApp(context.Background(), AppCommand{WriterGeneration: 1, ID: "minority-write", CallerNodeID: "node-1", CoordinatorEpoch: 1, ExpectedVersion: 1, Payload: []byte("100")})
	if err == nil {
		t.Fatal("isolated old coordinator committed a write")
	}
	if _, err = original.ReadState(context.Background()); err == nil {
		t.Fatal("isolated old coordinator authorized a read")
	}
	state, err = majority.ReadState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	result, err := majority.ApplyApp(context.Background(), AppCommand{WriterGeneration: 1, ID: "majority-write", CallerNodeID: state.Coordinator.NodeID, CoordinatorEpoch: state.Coordinator.Epoch, ExpectedVersion: state.AppVersion, Payload: []byte("2")})
	if err != nil || string(result.Data) != "3" {
		t.Fatalf("majority did not resume correctly: %+v %v", result, err)
	}
	if len(state.Voters) != 3 {
		t.Fatal("network partition shrank membership")
	}
	network.isolate("")
	eventually(t, 5*time.Second, func() bool { return original.Status().Coordinator.Epoch == 2 && original.Status().AppVersion == 2 })
	if value := c.configs["node-1"].Application.(*durableCounter).value(); value != 3 {
		t.Fatalf("healed replica diverged: %d", value)
	}
	_, err = majority.ApplyApp(context.Background(), AppCommand{WriterGeneration: 1, ID: "reconnected-old-epoch", CallerNodeID: "node-1", CoordinatorEpoch: 1, ExpectedVersion: 2, Payload: []byte("100")})
	if !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("reconnected stale writer accepted: %v", err)
	}
}

func TestManualTransferRejectsNodeThatCannotDemonstrateAppliedProgress(t *testing.T) {
	c, network := newPartitionCluster(t)
	leader := c.leader()
	network.isolate("node-3")
	_, err := leader.Transfer(context.Background(), TransferRequest{ID: "to-unreachable", Actor: "user", ExpectedEpoch: 1, TargetNodeID: "node-3"})
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("unreachable target was accepted: %v", err)
	}
	if leader.Status().Coordinator != (Assignment{NodeID: "node-1", Epoch: 1}) {
		t.Fatal("failed transfer changed coordinator")
	}
}
