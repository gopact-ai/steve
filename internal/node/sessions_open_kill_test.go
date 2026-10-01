package node

import (
	"context"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/nodewire"
)

func TestOpenKillStopsReservedSessionWhileNativeInitializeIsPending(t *testing.T) {
	s := NewServer(ServerConfig{Name: "worker", StateDir: t.TempDir(), Harnesses: map[string]HarnessSpec{"slow": {Command: "/bin/sh", Args: []string{"-c", "exec sleep 30"}}}, SessionAuthorizer: &sessionAuthorityTest{epoch: 1, writer: 1}})
	if err := s.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer s.sessions.Close()
	req := nodeSessionRequest("open")
	req.Harness, req.CommandID, req.Workdir = "slow", "original/open", t.TempDir()
	opened := make(chan error, 1)
	go func() { _, err := s.sessions.Do(t.Context(), "cluster-1", req); opened <- err }()
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer cancel()
	check := req
	check.Action = "inspect-open"
	for {
		state, err := s.sessions.Do(ctx, "cluster-1", check)
		if err == nil && state.State == "opening" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("original native open never reserved")
		case <-time.After(time.Millisecond):
		}
	}
	check.Action = "kill"
	began := time.Now()
	proof, err := s.sessions.Do(ctx, "cluster-1", check)
	if err != nil || !proof.ProcessStopped || proof.State != "closed" {
		t.Fatalf("cancel failed to fence pending native initialize: %+v %v", proof, err)
	}
	if time.Since(began) > 3*time.Second {
		t.Fatal("immediate kill waited for graceful timeout")
	}
	select {
	case <-opened:
	case <-ctx.Done():
		t.Fatal("native open outlived confirmed cancellation")
	}
	if state, err := s.sessions.Do(ctx, "cluster-1", req); err == nil && (state.State != "closed" || !state.ProcessStopped) {
		t.Fatal("cancelled native open could be restarted")
	}
}

func TestOpenKillFencesLateNativeCreationAcrossNodeRestart(t *testing.T) {
	authority := &sessionAuthorityTest{epoch: 1, writer: 1}
	cfg := ServerConfig{Name: "worker", StateDir: t.TempDir(), WorkspaceRoot: t.TempDir(), Harnesses: map[string]HarnessSpec{"mock": {Command: buildMockAgent(t)}}, SessionAuthorizer: authority}
	s := NewServer(cfg)
	if err := s.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer s.sessions.Close()
	req := nodeSessionRequest("inspect-open")
	req.CommandID, req.Harness, req.Workdir = "original/open", "mock", t.TempDir()
	if _, err := s.sessions.Do(t.Context(), "cluster-1", req); err == nil {
		t.Fatal("absence was treated as a native receipt")
	}
	req.Action = "kill"
	proof, err := s.sessions.Do(t.Context(), "cluster-1", req)
	if err != nil || proof.OpenReceipt == nil || !proof.OpenReceipt.CancelledBeforeOpen || !proof.ProcessStopped || proof.State != "closed" {
		t.Fatalf("original open not fenced: %+v %v", proof, err)
	}
	again, err := s.sessions.Do(t.Context(), "cluster-1", req)
	if err != nil || again.Sequence != proof.Sequence || again.ID != proof.ID {
		t.Fatalf("lost cancellation response was not idempotent: %+v %v", again, err)
	}
	req.Action = "open"
	if _, err := s.sessions.Do(t.Context(), "cluster-1", req); err == nil {
		t.Fatal("late open escaped durable cancellation")
	}
	s.sessions.Close()
	next := NewServer(cfg)
	if err := next.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer next.sessions.Close()
	authority.mu.Lock()
	authority.epoch, authority.writer = 2, 2
	authority.mu.Unlock()
	req.Authority.CoordinatorNodeID, req.Authority.CoordinatorEpoch, req.Authority.WriterGeneration = "hub-b", 2, 2
	if _, err := next.sessions.Do(t.Context(), "cluster-1", req); err == nil {
		t.Fatal("restart and new coordinator bypassed cancelled open")
	}
	req.Action = "inspect-open"
	proof, err = next.sessions.Do(t.Context(), "cluster-1", req)
	if err != nil || proof.OpenReceipt == nil || !proof.OpenReceipt.CancelledBeforeOpen {
		t.Fatalf("restart lost cancellation evidence: %+v %v", proof, err)
	}
}

func TestOpenKillWinsAgainstAlreadyAuthorizedDelayedOpen(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	base := &sessionAuthorityTest{epoch: 1, writer: 1}
	verifier := sessionAuthorizerFunc(func(ctx context.Context, principal string, a nodewire.SessionAuthority, b nodewire.SessionBinding, action nodewire.SessionAction) error {
		if err := base.AuthorizeNodeSession(ctx, principal, a, b, action); err != nil {
			return err
		}
		if action == "open" {
			close(entered)
			<-release
		}
		return nil
	})
	s := NewServer(ServerConfig{Name: "worker", StateDir: t.TempDir(), Harnesses: map[string]HarnessSpec{"mock": {Command: "/must-not-run"}}, SessionAuthorizer: verifier})
	if err := s.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer s.sessions.Close()
	req := nodeSessionRequest("open")
	req.Harness, req.CommandID, req.Workdir = "mock", "original/open", t.TempDir()
	result := make(chan error, 1)
	go func() { _, err := s.sessions.Do(t.Context(), "cluster-1", req); result <- err }()
	<-entered
	stop := req
	stop.Action = "kill"
	proof, err := s.sessions.Do(t.Context(), "cluster-1", stop)
	close(release)
	if err != nil || proof.OpenReceipt == nil || !proof.OpenReceipt.CancelledBeforeOpen {
		t.Fatalf("cancel did not win original open reservation: %+v %v", proof, err)
	}
	if err := <-result; err == nil {
		t.Fatal("previously authorized open created an agent after cancellation")
	}
}

func TestOpenKillDoesNotConfirmARefusedTombstone(t *testing.T) {
	s := NewServer(ServerConfig{Name: "worker", StateDir: t.TempDir(), SessionAuthorizer: &sessionAuthorityTest{epoch: 1, writer: 1}})
	if err := s.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer s.sessions.Close()
	records, err := s.sessions.recordsStore()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := records.db.Exec(`CREATE TRIGGER reject_tombstone BEFORE INSERT ON sessions BEGIN SELECT RAISE(ABORT, 'write refused'); END`); err != nil {
		t.Fatal(err)
	}
	req := nodeSessionRequest("kill")
	req.CommandID, req.Harness = "original/open", "mock"
	state, err := s.sessions.Do(t.Context(), "cluster-1", req)
	if err == nil || state.ProcessStopped {
		t.Fatalf("refused tombstone confirmed: %+v %v", state, err)
	}
	id := nodewire.SessionOpenID(req.Authority.ClusterID, req.Binding.NodeID, req.Binding.AttemptID, req.CommandID, req.Harness)
	_, found, err := records.read(id, "")
	if err != nil || found {
		t.Fatalf("refused tombstone survived: %v %v", found, err)
	}
}

func TestOpenKillRejectsUntrustedExecutionClaims(t *testing.T) {
	s := NewServer(ServerConfig{Name: "worker", StateDir: t.TempDir(), SessionAuthorizer: &sessionAuthorityTest{epoch: 1, writer: 1}})
	if err := s.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer s.sessions.Close()
	for _, which := range []string{"principal", "cluster", "attempt", "node", "command", "harness"} {
		t.Run(which, func(t *testing.T) {
			req := nodeSessionRequest("kill")
			req.CommandID, req.Harness = "original/open", "mock"
			principal := "cluster-1"
			switch which {
			case "principal":
				principal = "outsider"
			case "cluster":
				req.Authority.ClusterID = "other"
			case "attempt":
				req.Binding.AttemptID = "other"
			case "node":
				req.Binding.NodeID = "other"
			case "command":
				req.CommandID = ""
			case "harness":
				req.Harness = ""
			}
			if state, err := s.sessions.Do(t.Context(), principal, req); err == nil || state.ProcessStopped {
				t.Fatalf("invalid %s confirmed: %+v %v", which, state, err)
			}
		})
	}
}
