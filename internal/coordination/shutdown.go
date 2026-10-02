package coordination

import (
	"sync"

	"github.com/hashicorp/raft"
)

// Transport callbacks are not part of Raft's internal worker group. Seal their
// admission before shutdown, and drain accepted callbacks before closing storage.
type heartbeatGate struct {
	mu       sync.Mutex
	sealed   bool
	accepted sync.WaitGroup
}

func (g *heartbeatGate) wrap(handler func(raft.RPC)) func(raft.RPC) {
	return func(rpc raft.RPC) {
		g.mu.Lock()
		if g.sealed {
			g.mu.Unlock()
			rpc.Respond(nil, raft.ErrRaftShutdown)
			return
		}
		g.accepted.Add(1)
		g.mu.Unlock()
		defer g.accepted.Done()
		handler(rpc)
	}
}

func (g *heartbeatGate) seal() {
	g.mu.Lock()
	g.sealed = true
	g.mu.Unlock()
}

func (t addressTransport) SetHeartbeatHandler(handler func(raft.RPC)) {
	if handler == nil {
		t.NetworkTransport.SetHeartbeatHandler(nil)
		return
	}
	t.NetworkTransport.SetHeartbeatHandler(t.heartbeats.wrap(handler))
}

// A failure may initiate shutdown before Close. Keep its original future:
// calling Raft.Shutdown again returns a future that does not wait for its workers.
// start never waits, so the failure observer cannot wait for itself to finish.
type raftShutdown struct {
	heartbeats heartbeatGate
	once       sync.Once
	future     raft.Future
}

func (s *raftShutdown) start(node *raft.Raft) raft.Future {
	s.once.Do(func() {
		s.heartbeats.seal()
		s.future = node.Shutdown()
	})
	return s.future
}

func (s *raftShutdown) wait(node *raft.Raft) error {
	future := s.start(node)
	// start sealed admission under the same lock used by Add, so no future
	// callback can register while this wait is in progress.
	s.heartbeats.accepted.Wait()
	return future.Error()
}
