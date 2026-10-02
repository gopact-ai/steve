package coordination

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/raft"
)

func TestHeartbeatGateRejectsPreviouslyCapturedHandlers(t *testing.T) {
	var gate heartbeatGate
	transport := addressTransport{heartbeats: &gate}
	copy := transport
	var calls atomic.Int32
	captured := copy.heartbeats.wrap(func(rpc raft.RPC) {
		calls.Add(1)
		// Starting shutdown from inside an accepted callback may seal admission,
		// but cannot wait on the callback itself or hold its entry lock.
		transport.heartbeats.seal()
		rpc.Respond(nil, nil)
	})
	first := make(chan raft.RPCResponse, 1)
	done := make(chan struct{})
	go func() { captured(raft.RPC{RespChan: first}); close(done) }()
	waitShutdownSignal(t, done, "callback sealing admission")
	if result := <-first; result.Error != nil {
		t.Fatal(result.Error)
	}
	later := make(chan raft.RPCResponse, 1)
	captured(raft.RPC{RespChan: later})
	if result := <-later; !errors.Is(result.Error, raft.ErrRaftShutdown) {
		t.Fatalf("captured callback after sealing = %v", result.Error)
	}
	if calls.Load() != 1 {
		t.Fatalf("sealed callback entered %d times", calls.Load())
	}
	gate.accepted.Wait()
}

func TestHeartbeatGateDrainsEveryAcceptedCallback(t *testing.T) {
	var gate heartbeatGate
	const callbacks = 8
	entered := make(chan struct{}, callbacks)
	release := make(chan struct{})
	var once sync.Once
	unpause := func() { once.Do(func() { close(release) }) }
	defer unpause()
	handler := gate.wrap(func(rpc raft.RPC) {
		entered <- struct{}{}
		<-release
		rpc.Respond(nil, nil)
	})
	responses := make(chan raft.RPCResponse, callbacks)
	for range callbacks {
		go handler(raft.RPC{RespChan: responses})
	}
	for range callbacks {
		waitShutdownSignal(t, entered, "accepted callback")
	}
	gate.seal()
	drained := make(chan struct{})
	go func() { gate.accepted.Wait(); close(drained) }()
	select {
	case <-drained:
		t.Fatal("admitted callbacks were not included in the drain")
	case <-time.After(20 * time.Millisecond):
	}
	late := make(chan raft.RPCResponse, 1)
	lateDone := make(chan struct{})
	go func() { handler(raft.RPC{RespChan: late}); close(lateDone) }()
	waitShutdownSignal(t, lateDone, "late callback rejection")
	if result := <-late; !errors.Is(result.Error, raft.ErrRaftShutdown) {
		t.Fatalf("new callback crossed closed admission: %v", result.Error)
	}
	unpause()
	waitShutdownSignal(t, drained, "all accepted callbacks returning")
	for range callbacks {
		if result := <-responses; result.Error != nil {
			t.Fatal(result.Error)
		}
	}
}
