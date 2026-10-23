package node

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/permission"
)

// lateStopTransport starts agents whose stop is confirmed a moment after
// they exit, as that of an agent whose process group the kill takes a
// moment to empty.
type lateStopTransport struct {
	inner   acphost.Transport
	started chan acphost.Process
}

func (tr lateStopTransport) Name() string { return "late-stop-mock" }
func (tr lateStopTransport) Start(ctx context.Context) (acphost.Process, error) {
	p, err := tr.inner.Start(ctx)
	if err != nil {
		return nil, err
	}
	tr.started <- p
	return &lateStopProcess{Process: p}, nil
}

type lateStopProcess struct {
	acphost.Process
	stopped atomic.Bool
}

func (p *lateStopProcess) Wait() error {
	err := p.Process.Wait()
	time.Sleep(50 * time.Millisecond)
	p.stopped.Store(p.Process.Stopped())
	return err
}

func (p *lateStopProcess) Stopped() bool { return p.stopped.Load() }

// An agent that exits mid-prompt fails the command, and once its process
// group has emptied the node records the process stopped: the session is
// not left interrupted, as one whose agent may still be writing.
func TestNodeSessionPromptOfAnAgentThatExitsRecordsItsStop(t *testing.T) {
	s := NewServer(ServerConfig{Name: "worker", StateDir: t.TempDir(), WorkspaceRoot: t.TempDir(), SessionAuthorizer: &sessionAuthorityTest{epoch: 1, writer: 1}})
	if err := s.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer s.sessions.Close()
	broker, _ := permission.New("read")
	started := make(chan acphost.Process, 1)
	host := acphost.New(acphost.Config{NoRestart: true, Permission: broker, Transport: lateStopTransport{inner: acphost.LocalTransport{Command: buildMockAgent(t), ProcessDir: t.TempDir()}, started: started}})
	native, generation, err := host.OpenSession(t.Context(), "", acphost.SessionConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	agent := <-started
	req := nodeSessionRequest("prompt")
	req.ID = "ns_" + sessionHash("agent-exits")
	one := &ownedSession{service: s.sessions, host: host, changed: make(chan struct{}), waiters: map[string]chan struct{}{}}
	record := sessionRecord{Format: 1, ClusterID: req.Authority.ClusterID, Authority: req.Authority, UpstreamID: string(native), Generation: generation, State: nodewire.SessionState{ID: req.ID, Binding: req.Binding, Harness: "mock", State: "idle"}, Commands: map[string]nodewire.SessionCommand{}, CommandHashes: map[string]string{}}
	if err := one.commitLocked(record); err != nil {
		t.Fatal(err)
	}
	s.sessions.sessions[req.ID] = one
	req.CommandID, req.InputSequence, req.Text = "input-1", 1, "ignore-cancel"
	if _, err := s.sessions.Do(t.Context(), "cluster-1", req); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); one.state(req.CommandID).Progress.Answer == ""; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the agent did not start the prompt")
		}
	}
	agent.Kill()
	one.mu.Lock()
	done := one.runDone
	one.mu.Unlock()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the prompt of an agent that exited did not end")
	}
	state := one.state(req.CommandID)
	if state.Command == nil || state.Command.Error == "" || !state.Command.Settled || !state.Command.ProcessStopped || state.Command.State == nodewire.SessionCommandUncertain {
		t.Fatalf("command of an agent that exited = %+v", state.Command)
	}
	if !state.ProcessStopped || state.State == nodewire.SessionInterrupted {
		t.Fatalf("session of an agent that exited: state=%s processStopped=%v", state.State, state.ProcessStopped)
	}
}
