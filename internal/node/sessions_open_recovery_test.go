package node

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/nodewire"
)

func TestOpenCancellationStopsReservedSessionWhileNativeInitializeIsPending(t *testing.T) {
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
	check.Action = "cancel-open"
	proof, err := s.sessions.Do(ctx, "cluster-1", check)
	if err != nil || !proof.ProcessStopped || proof.State != "closed" {
		t.Fatalf("cancel failed to fence pending native initialize: %+v %v", proof, err)
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

func TestOpenCancellationFencesLateNativeCreationAcrossNodeRestart(t *testing.T) {
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
	req.Action = "cancel-open"
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

func TestOpenCancellationWinsAgainstAlreadyAuthorizedDelayedOpen(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	base := &sessionAuthorityTest{epoch: 1, writer: 1}
	verifier := sessionAuthorizerFunc(func(ctx context.Context, principal string, a nodewire.SessionAuthority, b nodewire.SessionBinding, action string) error {
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
	stop.Action = "cancel-open"
	proof, err := s.sessions.Do(t.Context(), "cluster-1", stop)
	close(release)
	if err != nil || proof.OpenReceipt == nil || !proof.OpenReceipt.CancelledBeforeOpen {
		t.Fatalf("cancel did not win original open reservation: %+v %v", proof, err)
	}
	if err := <-result; err == nil {
		t.Fatal("previously authorized open created an agent after cancellation")
	}
}

func TestOpenRecoveryFindsAndStopsOriginalNativeSession(t *testing.T) {
	server := startNode(t, ServerConfig{Name: "worker", Token: "open-recovery", StateDir: t.TempDir(), WorkspaceRoot: t.TempDir(), Harnesses: map[string]HarnessSpec{"mock": {Command: buildMockAgent(t)}}, SessionAuthorizer: CoordinatorSessionAuthorizer{}})
	registry := NewRegistry("cluster-1", map[string]Config{"worker": {Addr: server.Addr(), Token: "open-recovery"}})
	defer registry.Close()
	base := &sessionAuthorityTest{epoch: 1, writer: 1}
	registry.SetSessionAuthorizer(func(ctx context.Context, node string, a nodewire.SessionAuthority, b nodewire.SessionBinding, action string) error {
		if node != b.NodeID {
			return errors.New("wrong node")
		}
		return base.AuthorizeNodeSession(ctx, "cluster-1", a, b, action)
	})
	req := nodeSessionRequest("open")
	req.Harness, req.CommandID, req.Workdir = "mock", "original/open", t.TempDir()
	created, err := registry.NodeSession(t.Context(), "worker", req)
	if err != nil {
		t.Fatal(err)
	}
	registry.Close()
	registry = NewRegistry("cluster-1", map[string]Config{"worker": {Addr: server.Addr(), Token: "open-recovery"}})
	defer registry.Close()
	registry.SetSessionAuthorizer(func(ctx context.Context, _ string, a nodewire.SessionAuthority, b nodewire.SessionBinding, action string) error {
		return base.AuthorizeNodeSession(ctx, "cluster-1", a, b, action)
	})
	req.Action = "inspect-open"
	found, err := registry.NodeSession(t.Context(), "worker", req)
	if err != nil || found.ID != created.ID || found.Command != nil || found.InputAccepted != 0 {
		t.Fatalf("inspection replaced or dispatched original native preparation: %+v %v", found, err)
	}
	wrong := req
	wrong.Binding.TaskEpoch++
	if _, err := registry.NodeSession(t.Context(), "worker", wrong); err == nil {
		t.Fatal("wrong execution recovered original open")
	}
	req.Action = "cancel-open"
	stopped, err := registry.NodeSession(t.Context(), "worker", req)
	if err != nil || stopped.ID != created.ID || !stopped.ProcessStopped || stopped.OpenReceipt == nil || stopped.OpenReceipt.CancelledBeforeOpen {
		t.Fatalf("original native process not stopped: %+v %v", stopped, err)
	}
	req.Action = "inspect-open"
	archived, err := registry.NodeSession(t.Context(), "worker", req)
	if err != nil || archived.ID != created.ID || !archived.ProcessStopped {
		t.Fatalf("closed original receipt lost: %+v %v", archived, err)
	}
}

func TestOpenCancellationAfterNodeRestartRequiresPersistedProcessStopEvidence(t *testing.T) {
	for _, known := range []bool{false, true} {
		t.Run(map[bool]string{false: "unknown-process", true: "stopped-process"}[known], func(t *testing.T) {
			cfg := ServerConfig{Name: "worker", StateDir: t.TempDir(), SessionAuthorizer: &sessionAuthorityTest{epoch: 1, writer: 1}}
			s := NewServer(cfg)
			if err := s.startSessions(t.Context()); err != nil {
				t.Fatal(err)
			}
			req := nodeSessionRequest("cancel-open")
			req.Harness, req.CommandID = "mock", "original/open"
			id := nodewire.SessionOpenID(req.Authority.ClusterID, "worker", req.Binding.AttemptID, req.CommandID, req.Harness)
			one := &ownedSession{service: s.sessions, changed: make(chan struct{})}
			if err := one.commitLocked(sessionRecord{Format: 1, ClusterID: req.Authority.ClusterID, Authority: req.Authority, OpenID: req.CommandID, State: nodewire.SessionState{ID: id, Binding: req.Binding, Harness: req.Harness, State: "interrupted", ProcessStopped: known}, Commands: map[string]nodewire.SessionCommand{}, CommandHashes: map[string]string{}}); err != nil {
				t.Fatal(err)
			}
			s.sessions.Close()
			next := NewServer(cfg)
			if err := next.startSessions(t.Context()); err != nil {
				t.Fatal(err)
			}
			defer next.sessions.Close()
			state, err := next.sessions.Do(t.Context(), "cluster-1", req)
			if !known {
				if err == nil {
					t.Fatal("restarted node invented physical process stop")
				}
				return
			}
			if err != nil || state.State != "closed" || !state.ProcessStopped || state.OpenReceipt.CancelledBeforeOpen {
				t.Fatalf("persisted process stop could not close original open: %+v %v", state, err)
			}
		})
	}
}
