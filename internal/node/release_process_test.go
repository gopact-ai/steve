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

	"github.com/gopact-ai/steve/internal/nodewire"
)

type waitingReleaseProcess struct {
	waitStarted, allowExit, killed chan struct{}
	killOnce, exitOnce             sync.Once
	waitErr                        error
}

func (p *waitingReleaseProcess) Stdout() io.ReadCloser { return io.NopCloser(strings.NewReader("")) }
func (p *waitingReleaseProcess) Stdin() io.WriteCloser { return releaseDiscardWriter{} }
func (p *waitingReleaseProcess) Wait() error           { close(p.waitStarted); <-p.allowExit; return p.waitErr }
func (p *waitingReleaseProcess) Kill()                 { p.killOnce.Do(func() { close(p.killed) }) }
func (p *waitingReleaseProcess) finish()               { p.exitOnce.Do(func() { close(p.allowExit) }) }

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
