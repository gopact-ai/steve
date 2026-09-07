package node

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/gopact-ai/steve/internal/nodewire"
)

type sessionAuthorityTest struct {
	mu            sync.Mutex
	epoch, writer uint64
	denyStart     bool
}

func (a *sessionAuthorityTest) AuthorizeNodeSession(_ context.Context, principal string, authority nodewire.SessionAuthority, binding nodewire.SessionBinding, action string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if action == "start" && a.denyStart {
		return errors.New("execution lease expired or attempt quarantined")
	}
	if principal != "cluster-1" || authority.ClusterID != "cluster-1" || authority.CoordinatorEpoch != a.epoch || authority.WriterGeneration != a.writer || binding.NodeID != "worker" || binding.AttemptID != "attempt-1" {
		return errors.New("not the committed coordinator or execution")
	}
	return nil
}

func nodeSessionRequest(action string) nodewire.SessionRequest {
	return nodewire.SessionRequest{Action: action, Authority: nodewire.SessionAuthority{ClusterID: "cluster-1", CoordinatorNodeID: "hub-a", CoordinatorEpoch: 1, WriterGeneration: 1}, Binding: nodewire.SessionBinding{ProjectID: "p", SessionID: "conversation-1", TaskID: "task-1", AttemptID: "attempt-1", NodeID: "worker", ExecutionEpoch: 1, TaskEpoch: 1}}
}

func TestNodeSessionOwnsPendingQuestionAcrossCoordinatorReplacement(t *testing.T) {
	authority := &sessionAuthorityTest{epoch: 1, writer: 1}
	s := NewServer(ServerConfig{Name: "worker", StateDir: t.TempDir(), WorkspaceRoot: t.TempDir(), Harnesses: map[string]HarnessSpec{"mock": {Command: buildMockAgent(t)}}, SessionAuthorizer: authority})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := s.startSessions(ctx); err != nil {
		t.Fatal(err)
	}
	if s.sessions == nil {
		t.Fatal("node did not initialize owned sessions")
	}
	defer s.sessions.Close()
	req := nodeSessionRequest("open")
	req.Harness = "mock"
	req.Workdir = t.TempDir()
	req.CommandID = "open-1"
	state, err := s.sessions.Do(ctx, "cluster-1", req)
	if err != nil {
		t.Fatal(err)
	}
	req.ID = state.ID
	req.Action = "prompt"
	req.CommandID = "input-1"
	req.InputSequence = 1
	req.Text = "askme"
	state, err = s.sessions.Do(ctx, "cluster-1", req)
	if err != nil {
		t.Fatal(err)
	}
	if state.Command == nil || state.Command.DispatchState != "not-dispatched" {
		t.Fatalf("accepted input lacks a durable no-dispatch receipt: %+v", state.Command)
	}
	for i := 0; i < 100 && len(state.Questions) == 0; i++ {
		poll := req
		poll.Action = "poll"
		poll.After = state.Sequence
		poll.WaitMS = 50
		state, err = s.sessions.Do(ctx, "cluster-1", poll)
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(state.Questions) != 1 || state.Questions[0].State != "pending" {
		t.Fatalf("pending question: %+v", state)
	}
	if state.Command == nil || state.Command.DispatchState != "dispatched" {
		t.Fatalf("native question lacks the persisted dispatch marker: %+v", state.Command)
	}
	questionID := state.Questions[0].ID
	authority.mu.Lock()
	authority.epoch = 2
	authority.writer = 2
	authority.mu.Unlock()
	answer := req
	answer.Action = "answer"
	answer.QuestionID = questionID
	answer.Answer = &nodewire.SessionAnswer{CommandID: "answer-1", Decision: "accept", Choice: "Blue"}
	if _, err := s.sessions.Do(ctx, "cluster-1", answer); err == nil {
		t.Fatal("old coordinator answered")
	}
	req.Authority.CoordinatorEpoch = 2
	req.Authority.WriterGeneration = 2
	req.Authority.CoordinatorNodeID = "hub-b"
	req.Action = "attach"
	state, err = s.sessions.Do(ctx, "cluster-1", req)
	if err != nil {
		t.Fatal(err)
	}
	if state.Questions[0].ID != questionID {
		t.Fatal("reattach replaced original question")
	}
	answer.Authority = req.Authority
	for range 2 {
		if _, err := s.sessions.Do(ctx, "cluster-1", answer); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 100 && (state.Command == nil || !state.Command.Settled); i++ {
		req.Action = "poll"
		req.After = state.Sequence
		req.WaitMS = 50
		state, err = s.sessions.Do(ctx, "cluster-1", req)
		if err != nil {
			t.Fatal(err)
		}
	}
	if state.Command == nil || !state.Command.Settled || !strings.Contains(state.Command.Output, "accept:Blue") {
		t.Fatalf("original execution did not continue: %+v", state.Command)
	}
	req.Action = "prompt"
	if got, err := s.sessions.Do(ctx, "cluster-1", req); err != nil || got.InputAccepted != 1 {
		t.Fatalf("input retry replayed: %+v %v", got, err)
	}
	req.Text = "different"
	if _, err := s.sessions.Do(ctx, "cluster-1", req); err == nil {
		t.Fatal("input ID accepted changed payload")
	}
}

func TestNodeSessionServiceRestartNeverReplaysUncertainNativePrompt(t *testing.T) {
	authority := &sessionAuthorityTest{epoch: 1, writer: 1}
	cfg := ServerConfig{Name: "worker", StateDir: t.TempDir(), WorkspaceRoot: t.TempDir(), Harnesses: map[string]HarnessSpec{"mock": {Command: buildMockAgent(t)}}, SessionAuthorizer: authority}
	s := NewServer(cfg)
	if err := s.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	if s.sessions == nil {
		t.Fatal("node session owner not initialized")
	}
	req := nodeSessionRequest("open")
	req.Harness = "mock"
	req.Workdir = t.TempDir()
	req.CommandID = "open-slow"
	state, err := s.sessions.Do(t.Context(), "cluster-1", req)
	if err != nil {
		t.Fatal(err)
	}
	req.ID = state.ID
	req.Action = "prompt"
	req.CommandID = "input-slow"
	req.InputSequence = 1
	req.Text = "slow"
	if _, err := s.sessions.Do(t.Context(), "cluster-1", req); err != nil {
		t.Fatal(err)
	}
	s.sessions.Close()
	restarted := NewServer(cfg)
	if err := restarted.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer restarted.sessions.Close()
	req.Action = "attach"
	state, err = restarted.sessions.Do(t.Context(), "cluster-1", req)
	if err != nil {
		t.Fatal(err)
	}
	if state.State != "interrupted" || state.Command == nil || (state.Command.State != "uncertain" && state.Command.State != "cancelled") {
		t.Fatalf("restart invented a live native session: %+v", state)
	}
	req.Action = "prompt"
	if got, err := restarted.sessions.Do(t.Context(), "cluster-1", req); err == nil && got.Command.State == "running" {
		t.Fatal("restart replayed native prompt")
	}
}

func TestNodeSessionCapabilityRequiresAuthority(t *testing.T) {
	s := NewServer(ServerConfig{Name: "worker", StateDir: t.TempDir(), WorkspaceRoot: t.TempDir()})
	if err := s.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	if s.sessions != nil {
		t.Fatal("sessions enabled without authorization")
	}
}

func TestNodeSessionCreationRequiresStartAuthorityButExistingSessionCanBeObserved(t *testing.T) {
	authority := &sessionAuthorityTest{epoch: 1, writer: 1}
	s := NewServer(ServerConfig{Name: "worker", StateDir: t.TempDir(), WorkspaceRoot: t.TempDir(), Harnesses: map[string]HarnessSpec{"mock": {Command: buildMockAgent(t)}}, SessionAuthorizer: authority})
	if err := s.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer s.sessions.Close()
	req := nodeSessionRequest("open")
	req.CommandID = "initial-open"
	req.Workdir = t.TempDir()
	req.Harness = "mock"
	initial, err := s.sessions.Do(t.Context(), "cluster-1", req)
	if err != nil {
		t.Fatal(err)
	}
	authority.mu.Lock()
	authority.denyStart = true
	authority.mu.Unlock()
	req.ID = initial.ID
	if state, err := s.sessions.Do(t.Context(), "cluster-1", req); err != nil || state.ID != initial.ID {
		t.Fatalf("expired execution cannot observe its existing session: %+v %v", state, err)
	}
	req.ID = ""
	req.CommandID = "new-open-without-authority"
	if _, err := s.sessions.Do(t.Context(), "cluster-1", req); err == nil {
		t.Fatal("observation authority created a new native process")
	}
	if len(s.sessions.sessions) != 1 {
		t.Fatal("rejected start allocated a native session")
	}
}
