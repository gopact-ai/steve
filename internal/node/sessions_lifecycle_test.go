package node

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/permission"
)

type sessionRPCGate struct {
	operation        string
	entered, release chan struct{}
	once             sync.Once
}

func (g *sessionRPCGate) wait() { g.once.Do(func() { close(g.entered) }); <-g.release }

type sessionGateTransport struct {
	inner acphost.Transport
	gate  *sessionRPCGate
}

func (tr sessionGateTransport) Name() string { return "gated-mock" }
func (tr sessionGateTransport) Start(ctx context.Context) (acphost.Process, error) {
	p, err := tr.inner.Start(ctx)
	if err != nil {
		return nil, err
	}
	return &sessionGateProcess{Process: p, gate: tr.gate}, nil
}

type sessionGateProcess struct {
	acphost.Process
	gate *sessionRPCGate
}

func (p *sessionGateProcess) Stdout() io.ReadCloser {
	return sessionGateReader{ReadCloser: p.Process.Stdout(), gate: p.gate}
}
func (p *sessionGateProcess) Stdin() io.WriteCloser {
	return sessionGateWriter{WriteCloser: p.Process.Stdin(), gate: p.gate}
}
func (p *sessionGateProcess) Stopped() bool {
	if evidence, ok := p.Process.(interface{ Stopped() bool }); ok {
		return evidence.Stopped()
	}
	return false
}

type sessionGateReader struct {
	io.ReadCloser
	gate *sessionRPCGate
}

func (r sessionGateReader) Close() error {
	if r.gate.operation == "close" {
		r.gate.wait()
	}
	return r.ReadCloser.Close()
}

type sessionGateWriter struct {
	io.WriteCloser
	gate *sessionRPCGate
}

func (w sessionGateWriter) Write(data []byte) (int, error) {
	if w.gate.operation == "option" && bytes.Contains(data, []byte("session/set_config_option")) {
		w.gate.wait()
	}
	return w.WriteCloser.Write(data)
}

func TestNodeSessionLifecycleRPCReservesBeforeCallingNativeAgent(t *testing.T) {
	for _, operation := range []string{"close", "option"} {
		t.Run(operation, func(t *testing.T) {
			s := NewServer(ServerConfig{Name: "worker", StateDir: t.TempDir(), WorkspaceRoot: t.TempDir(), SessionAuthorizer: &sessionAuthorityTest{epoch: 1, writer: 1}})
			if err := s.startSessions(t.Context()); err != nil {
				t.Fatal(err)
			}
			defer s.sessions.Close()
			gate := &sessionRPCGate{operation: operation, entered: make(chan struct{}), release: make(chan struct{})}
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(gate.release) }) }
			defer release()
			broker, _ := permission.New("read")
			host := acphost.New(acphost.Config{NoRestart: true, Permission: broker, Transport: sessionGateTransport{inner: acphost.LocalTransport{Command: buildMockAgent(t), ProcessDir: t.TempDir()}, gate: gate}})
			native, generation, err := host.OpenSession(t.Context(), "", acphost.SessionConfig{Workdir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			req := nodeSessionRequest(operation)
			req.ID = "ns_" + sessionHash(operation)
			one := &ownedSession{service: s.sessions, host: host, changed: make(chan struct{}), waiters: map[string]chan struct{}{}}
			record := sessionRecord{Format: 1, ClusterID: req.Authority.ClusterID, Authority: req.Authority, UpstreamID: string(native), Generation: generation, State: nodewire.SessionState{ID: req.ID, Binding: req.Binding, Harness: "mock", State: "idle"}, Commands: map[string]nodewire.SessionCommand{}, CommandHashes: map[string]string{}}
			if err := one.commitLocked(record); err != nil {
				t.Fatal(err)
			}
			s.sessions.sessions[req.ID] = one
			req.OptionID = "model"
			req.OptionValue = "mock-deep"
			done := make(chan error, 1)
			go func() { _, err := s.sessions.Do(t.Context(), "cluster-1", req); done <- err }()
			select {
			case <-gate.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("native lifecycle call did not enter gate")
			}
			prompt := req
			prompt.Action = "prompt"
			prompt.CommandID = "new-input"
			prompt.InputSequence = 1
			prompt.Text = "must not enter during lifecycle call"
			if _, err := s.sessions.Do(t.Context(), "cluster-1", prompt); err == nil {
				t.Fatal("native lifecycle call admitted a concurrent prompt")
			}
			release()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("native lifecycle call did not finish")
			}
		})
	}
}
