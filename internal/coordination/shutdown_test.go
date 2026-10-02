package coordination

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/raft"
)

// Pause an accepted RPC through the real transport's response-header callback.
// The Raft shutdown check has already run when appendEntries asks for its address.
type shutdownStream struct {
	raft.StreamLayer
	entered   chan struct{}
	released  chan struct{}
	enter     sync.Once
	leave     sync.Once
	bootstrap chan struct{}
	proceed   chan struct{}
	boot      sync.Once
}

func (s *shutdownStream) Addr() net.Addr {
	callers := make([]uintptr, 32)
	n := runtime.Callers(2, callers)
	frames := runtime.CallersFrames(callers[:n])
	var heartbeat, opening, newRaft bool
	for {
		frame, more := frames.Next()
		heartbeat = heartbeat || frame.Function == "github.com/hashicorp/raft.(*Raft).processHeartbeat"
		opening = opening || frame.Function == "github.com/gopact-ai/steve/internal/coordination.Open"
		newRaft = newRaft || frame.Function == "github.com/hashicorp/raft.NewRaft"
		if !more {
			break
		}
	}
	if heartbeat {
		s.enter.Do(func() { close(s.entered) })
		<-s.released
	}
	if s.bootstrap != nil && opening && !newRaft {
		s.boot.Do(func() { close(s.bootstrap) })
		<-s.proceed
		return emptyShutdownAddress{}
	}
	return s.StreamLayer.Addr()
}

func (s *shutdownStream) release() { s.leave.Do(func() { close(s.released) }) }

type emptyShutdownAddress struct{}

func (emptyShutdownAddress) Network() string { return "tcp" }
func (emptyShutdownAddress) String() string  { return "" }

type shutdownTCPStream struct{ net.Listener }

func (s shutdownTCPStream) Dial(address raft.ServerAddress, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout("tcp", string(address), timeout)
}

func TestServiceCloseDrainsAcceptedHeartbeats(t *testing.T) {
	if scenario := os.Getenv("STEVE_SHUTDOWN_SCENARIO"); scenario != "" {
		runShutdownScenario(t, scenario)
		return
	}
	for _, scenario := range []string{"tcp", "tls", "failed-application", "bootstrap-error"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestServiceCloseDrainsAcceptedHeartbeats$", "-test.v", "-test.timeout=12s")
			child.Env = append(os.Environ(), "STEVE_SHUTDOWN_SCENARIO="+scenario, "STEVE_SHUTDOWN_DIRECTORY="+t.TempDir())
			out, err := child.CombinedOutput()
			if err != nil {
				t.Fatalf("shutdown did not drain %s: %v\n%s", scenario, err, out)
			}
			t.Logf("%s", out)
		})
	}
}

func shutdownFixture(t *testing.T, secure bool) (*shutdownStream, *raft.NetworkTransport, raft.ServerAddress) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := raft.ServerAddress(listener.Addr().String())
	var server raft.StreamLayer = shutdownTCPStream{listener}
	var sender *raft.NetworkTransport
	if secure {
		authority := newTestAuthority(t)
		a := authority.node(t, "shutdown-cluster", "target")
		b := authority.node(t, "shutdown-cluster", "sender")
		a.AuthorizePeer = func(i Identity) bool { return i.ClusterID == "shutdown-cluster" && i.NodeID == "sender" }
		b.AuthorizePeer = func(i Identity) bool { return i.ClusterID == "shutdown-cluster" && i.NodeID == "target" }
		server, err = NewTLSStreamLayer(listener, a, func(raft.ServerAddress) string { return "sender" }, nil)
		if err != nil {
			listener.Close()
			t.Fatal(err)
		}
		other, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			server.Close()
			t.Fatal(err)
		}
		client, err := NewTLSStreamLayer(other, b, func(raft.ServerAddress) string { return "target" }, nil)
		if err != nil {
			other.Close()
			server.Close()
			t.Fatal(err)
		}
		sender = raft.NewNetworkTransport(client, 1, 4*time.Second, io.Discard)
	} else {
		sender, err = raft.NewTCPTransport("127.0.0.1:0", nil, 1, 4*time.Second, io.Discard)
		if err != nil {
			server.Close()
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { sender.CloseStreams(); sender.Close(); server.Close() })
	stream := &shutdownStream{StreamLayer: server, entered: make(chan struct{}), released: make(chan struct{})}
	t.Cleanup(stream.release)
	return stream, sender, address
}

func shutdownConfig(stream raft.StreamLayer) Config {
	cfg := raft.DefaultConfig()
	cfg.HeartbeatTimeout, cfg.ElectionTimeout = time.Hour, time.Hour
	cfg.LeaderLeaseTimeout, cfg.SnapshotInterval = time.Second, time.Hour
	return Config{ClusterID: "shutdown-cluster", NodeID: "target", DataDir: os.Getenv("STEVE_SHUTDOWN_DIRECTORY"), StreamLayer: stream, RaftConfig: cfg, ApplyTimeout: time.Second, ProbeInterval: time.Hour, LogOutput: io.Discard}
}

func sendShutdownHeartbeat(sender *raft.NetworkTransport, address raft.ServerAddress) <-chan error {
	done := make(chan error, 1)
	go func() {
		request := raft.AppendEntriesRequest{RPCHeader: raft.RPCHeader{ProtocolVersion: raft.ProtocolVersionMax, ID: []byte("sender"), Addr: []byte(sender.LocalAddr())}, Term: 7}
		var reply raft.AppendEntriesResponse
		done <- sender.AppendEntries("target", address, &request, &reply)
	}()
	return done
}

func waitShutdownSignal(t *testing.T, signal <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(4 * time.Second):
		t.Fatalf("%s was not reached", what)
	}
}

func runShutdownScenario(t *testing.T, scenario string) {
	stream, sender, address := shutdownFixture(t, scenario == "tls")
	cfg := shutdownConfig(stream)
	if scenario == "bootstrap-error" {
		runBootstrapShutdown(t, cfg, stream, sender, address)
		return
	}
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stream.release(); s.Close() })
	if scenario == "failed-application" {
		runFailedApplicationShutdown(t, s, sender, address)
		return
	}
	done := sendShutdownHeartbeat(sender, address)
	waitShutdownSignal(t, stream.entered, "accepted heartbeat")
	if scenario == "tls" {
		// The callback has been decoded: even closing the actual TLS sockets
		// cannot substitute for waiting for its database use to finish.
		if err := stream.StreamLayer.Close(); err != nil {
			t.Fatal(err)
		}
	}
	closed := make(chan error, 2)
	go func() { closed <- s.Close() }()
	go func() { closed <- s.Close() }()
	assertShutdownWaits(t, s, closed, stream.release, 2)
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("heartbeat request survived closed service")
	}
}

func assertShutdownWaits(t *testing.T, s *Service, closed <-chan error, release func(), count int) {
	t.Helper()
	select {
	case err := <-closed:
		_, readErr := s.store.GetUint64([]byte("CurrentTerm"))
		t.Logf("close returned with an admitted operation blocked: close=%v store=%v", err, readErr)
		release()
		t.Fatal("close returned before the admitted operation finished")
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := s.store.GetUint64([]byte("CurrentTerm")); err != nil {
		t.Fatalf("durable store closed before the admitted operation: %v", err)
	}
	release()
	for range count {
		select {
		case err := <-closed:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(4 * time.Second):
			t.Fatal("close did not finish after the operation was released")
		}
	}
	if _, err := s.store.GetUint64([]byte("CurrentTerm")); err == nil || !strings.Contains(err.Error(), "database not open") {
		t.Fatalf("store not closed after shutdown: %v", err)
	}
}

func runFailedApplicationShutdown(t *testing.T, s *Service, sender *raft.NetworkTransport, address raft.ServerAddress) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unpause := func() { once.Do(func() { close(release) }) }
	observer := raft.NewObserver(nil, false, func(o *raft.Observation) bool {
		if request, ok := o.Data.(raft.RequestVoteRequest); ok && string(request.ID) == "shutdown-vote" {
			close(entered)
			<-release
		}
		return false
	})
	s.raft.RegisterObserver(observer)
	defer func() { unpause(); s.raft.DeregisterObserver(observer) }()
	done := make(chan error, 1)
	go func() {
		request := raft.RequestVoteRequest{RPCHeader: raft.RPCHeader{ProtocolVersion: raft.ProtocolVersionMax, ID: []byte("shutdown-vote"), Addr: []byte(sender.LocalAddr())}, Term: 9}
		var response raft.RequestVoteResponse
		done <- sender.RequestVote("target", address, &request, &response)
	}()
	waitShutdownSignal(t, entered, "Raft main-loop operation")
	s.fsm.mu.Lock()
	s.fsm.fail(errors.New("application failed"))
	s.fsm.mu.Unlock()
	retired := make(chan struct{})
	go func() { s.workers.Wait(); close(retired) }()
	waitShutdownSignal(t, retired, "failure shutdown initiator")
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	assertShutdownWaits(t, s, closed, unpause, 1)
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("vote request survived closed service")
	}
}

func runBootstrapShutdown(t *testing.T, cfg Config, stream *shutdownStream, sender *raft.NetworkTransport, address raft.ServerAddress) {
	stream.bootstrap, stream.proceed = make(chan struct{}), make(chan struct{})
	cfg.Bootstrap = true
	opened := make(chan error, 1)
	go func() {
		s, err := Open(cfg)
		if s != nil {
			err = errors.Join(err, s.Close())
		}
		opened <- err
	}()
	waitShutdownSignal(t, stream.bootstrap, "bootstrap address")
	done := sendShutdownHeartbeat(sender, address)
	waitShutdownSignal(t, stream.entered, "bootstrap heartbeat")
	close(stream.proceed)
	select {
	case err := <-opened:
		t.Logf("Open finished bootstrap cleanup with accepted heartbeat blocked: %v", err)
		stream.release()
		t.Fatal("bootstrap cleanup did not wait for the accepted heartbeat")
	case <-time.After(100 * time.Millisecond):
	}
	stream.release()
	select {
	case err := <-opened:
		if err == nil || !strings.Contains(err.Error(), "bootstrap cluster") {
			t.Fatalf("bootstrap failure was not reported: %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("bootstrap cleanup did not finish")
	}
	select {
	case err := <-done:
		t.Log(fmt.Sprintf("bootstrap heartbeat completed: %v", err))
	case <-time.After(4 * time.Second):
		t.Fatal("bootstrap heartbeat request did not finish")
	}
}
