package node

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/nodewire"
)

type waitingReleaseProcess struct {
	waitStarted, allowExit, killed chan struct{}
	killOnce, exitOnce             sync.Once
	waitErr                        error
}

func (p *waitingReleaseProcess) Stdout() io.ReadCloser   { return io.NopCloser(strings.NewReader("")) }
func (p *waitingReleaseProcess) Stdin() io.WriteCloser   { return releaseDiscardWriter{} }
func (p *waitingReleaseProcess) Wait() error             { close(p.waitStarted); <-p.allowExit; return p.waitErr }
func (p *waitingReleaseProcess) Exited() <-chan struct{} { return p.allowExit }
func (p *waitingReleaseProcess) Kill()                   { p.killOnce.Do(func() { close(p.killed) }) }
func (p *waitingReleaseProcess) finish()                 { p.exitOnce.Do(func() { close(p.allowExit) }) }

// Stopped is never evidence here: the tests want a process whose exit
// the node cannot vouch for.
func (p *waitingReleaseProcess) Stopped() bool { return false }

type releaseDiscardWriter struct{}

func (releaseDiscardWriter) Write(b []byte) (int, error) { return len(b), nil }
func (releaseDiscardWriter) Close() error                { return nil }

func releaseProofStream(t *testing.T, s *Server, id string) *nodewire.Stream {
	t.Helper()
	a, b := net.Pipe()
	client, server := nodewire.NewMux(a, true), nodewire.NewMux(b, false)
	finished := make(chan struct{})
	t.Cleanup(func() {
		client.Close()
		server.Close()
		select {
		case <-finished:
		case <-time.After(time.Second):
			t.Error("release handler did not stop after connection closed")
		}
	})
	go func() {
		defer close(finished)
		stream, err := server.Accept(t.Context())
		if err == nil {
			s.releaseProcess(stream)
		}
	}()
	stream, err := client.Open(nodewire.OpenRequest{Kind: nodewire.StreamRelease, Stream: id})
	if err != nil {
		t.Fatal(err)
	}
	return stream
}

func TestReleaseProcessAcknowledgesOnlyAfterWaitIncludingNonzeroExit(t *testing.T) {
	for _, waitErr := range []error{nil, errors.New("nonzero child exit")} {
		name := "zero-exit"
		if waitErr != nil {
			name = "nonzero-exit"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			s := NewServer(ServerConfig{StateDir: t.TempDir()})
			s.ctx = ctx
			proc := &waitingReleaseProcess{waitStarted: make(chan struct{}), allowExit: make(chan struct{}), killed: make(chan struct{}), waitErr: waitErr}
			p := &agentProcess{server: s, id: "delayed", proc: proc}
			s.processes[p.id] = p
			s.processWG.Add(1)
			go p.run(ctx)
			t.Cleanup(func() { proc.finish(); s.processWG.Wait() })
			<-proc.waitStarted
			stream := releaseProofStream(t, s, p.id)
			result := make(chan error, 1)
			go func() { result <- awaitExit(ctx, stream, "n") }()
			<-proc.killed
			select {
			case err := <-result:
				t.Fatalf("kill request acknowledged before Wait returned: %v", err)
			case <-time.After(40 * time.Millisecond):
			}
			proc.finish()
			select {
			case err := <-result:
				if err != nil {
					t.Fatal("confirmed child exit did not release successfully", err)
				}
			case <-time.After(time.Second):
				t.Fatal("release did not acknowledge verified exit")
			}
			p.mu.Lock()
			exit := p.exit
			p.mu.Unlock()
			if exit == "" {
				t.Fatal("ACK without recorded physical exit")
			}
		})
	}
}

func TestReleaseProcessUnknownIDNeverAcknowledgesExit(t *testing.T) {
	s := NewServer(ServerConfig{})
	s.ctx = t.Context()
	stream := releaseProofStream(t, s, "never-started")
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := awaitExit(ctx, stream, "n"); err == nil {
		t.Fatal("unknown process accepted as physically exited")
	}
}

func TestReleaseProcessCancellationDoesNotConfirmExit(t *testing.T) {
	for _, disconnect := range []bool{false, true} {
		name := "context"
		if disconnect {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			s := NewServer(ServerConfig{})
			s.ctx = ctx
			proc := &waitingReleaseProcess{killed: make(chan struct{})}
			s.processes["running"] = &agentProcess{id: "running", proc: proc}
			stream := releaseProofStream(t, s, "running")
			result := make(chan error, 1)
			go func() { result <- awaitExit(t.Context(), stream, "n") }()
			<-proc.killed
			if disconnect {
				stream.Close()
			} else {
				cancel()
			}
			select {
			case err := <-result:
				if err == nil {
					t.Fatal("cancelled release fabricated exit acknowledgement")
				}
			case <-time.After(time.Second):
				t.Fatal("release ignored serving cancellation")
			}
		})
	}
}

// lingeringProcess is an agent that has exited while a member of its
// process group does not stop, as one in uninterruptible sleep: its output
// is over and Exited is closed, but Wait returns, and the stop is
// confirmed, only once the group is let go.
type lingeringProcess struct {
	exited, settled chan struct{}
	settleOnce      sync.Once
}

func newLingeringProcess() *lingeringProcess {
	p := &lingeringProcess{exited: make(chan struct{}), settled: make(chan struct{})}
	close(p.exited)
	return p
}

func (p *lingeringProcess) Stdout() io.ReadCloser   { return io.NopCloser(strings.NewReader("")) }
func (p *lingeringProcess) Stdin() io.WriteCloser   { return releaseDiscardWriter{} }
func (p *lingeringProcess) Wait() error             { <-p.settled; return nil }
func (p *lingeringProcess) Exited() <-chan struct{} { return p.exited }
func (p *lingeringProcess) Kill()                   {}
func (p *lingeringProcess) settle()                 { p.settleOnce.Do(func() { close(p.settled) }) }

func (p *lingeringProcess) Stopped() bool {
	select {
	case <-p.settled:
		return true
	default:
		return false
	}
}

func busyProcesses(s *Server) int {
	s.restart.mu.Lock()
	defer s.restart.mu.Unlock()
	return s.restartStatusLocked().Processes
}

// An agent can exit while a member of its process group stays past every
// kill. The hub is told the agent is gone, so it can start another, but
// not that it exited: its stop is confirmed only once the group has
// stopped, by the release the hub sends, and until then the node counts
// the process as running.
func TestNodeTellsTheHubAnAgentExitedWhileItsGroupHasNotStopped(t *testing.T) {
	m := newMemoryNode(t, "/bin/cat")
	agent := newLingeringProcess()
	t.Cleanup(agent.settle)
	cfg := m.s.conf()
	// The release the hub sends waits this long for the node's answer.
	cfg.SessionGrace = time.Minute
	cfg.startAgent = func(context.Context, acphost.LocalTransport) (acphost.Process, error) { return agent, nil }
	m.s.cfg.Store(&cfg)
	r := memoryRegistry(t)
	connectMemory(t, m, r)
	p := startRemote(t, r)
	select {
	case <-p.Exited():
	case <-time.After(15 * time.Second):
		t.Fatal("the hub was not told the agent had exited while its process group did not stop")
	}
	if err := p.Wait(); err == nil || strings.HasPrefix(err.Error(), nodewire.ExitPrefix) {
		t.Fatalf("the hub was told %v, want an end that is not an exit", err)
	}
	if p.Stopped() {
		t.Fatal("the hub took the agent's end for its stop while its process group had not stopped")
	}
	if n := busyProcesses(m.s); n != 1 {
		t.Fatalf("the node counts %d running processes while an exited agent's group has not stopped, want 1", n)
	}
	// Closing is how the hub lets the process go; it sends the release.
	_ = p.Close()
	time.Sleep(100 * time.Millisecond)
	if p.Stopped() {
		t.Fatal("the release confirmed the stop while the agent's process group had not stopped")
	}
	agent.settle()
	waitFor(t, p.Stopped)
	if n := busyProcesses(m.s); n != 0 {
		t.Fatalf("the node counts %d running processes once the agent's group has stopped, want 0", n)
	}
}

// A node that stops does not wait for an exited agent's process group that
// does not stop: it stops without an exit recorded for the agent, so its
// stop stays unconfirmed.
func TestNodeStopsWithoutWaitingOutAnExitedAgentsGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s := NewServer(ServerConfig{StateDir: t.TempDir()})
	agent := newLingeringProcess()
	t.Cleanup(agent.settle)
	p := &agentProcess{server: s, id: "lingering", proc: agent}
	s.processes[p.id] = p
	s.processWG.Add(1)
	go p.run(ctx)
	cancel()
	stopped := make(chan struct{})
	go func() { s.processWG.Wait(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(15 * time.Second):
		t.Fatal("the node did not stop while an exited agent's process group did not")
	}
	p.mu.Lock()
	exit := p.exit
	p.mu.Unlock()
	if exit != "" {
		t.Fatalf("the node recorded %q for an agent whose process group had not stopped", exit)
	}
}
