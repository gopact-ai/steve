package node

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/harness"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/view"
)

func TestNodeSessionWireReattachesAfterOwningManagerAndConnectionClose(t *testing.T) {
	testNodeSessionReattachment(t, false)
}

func TestStandaloneNodeSessionReattachesQuestionAndFencesOldCoordinator(t *testing.T) {
	testNodeSessionReattachment(t, true)
}

func testNodeSessionReattachment(t *testing.T, standalone bool) {
	authority := &sessionAuthorityTest{epoch: 1, writer: 1}
	var verifier SessionAuthorizer = authority
	if standalone {
		verifier = CoordinatorSessionAuthorizer{}
	}
	server := startNode(t, ServerConfig{Name: "worker", Token: "session-test", StateDir: t.TempDir(), WorkspaceRoot: t.TempDir(), Harnesses: map[string]HarnessSpec{"mock": {Command: buildMockAgent(t)}}, SessionAuthorizer: verifier})
	registry := NewRegistry("cluster-1", map[string]Config{"worker": {Addr: server.Addr(), Token: "session-test"}})
	verify := func(ctx context.Context, node string, a nodewire.SessionAuthority, b nodewire.SessionBinding, action string) error {
		if node != "worker" {
			return errors.New("wrong authenticated node")
		}
		return authority.AuthorizeNodeSession(ctx, "cluster-1", a, b, action)
	}
	registry.SetSessionAuthorizer(verify)
	first, err := harness.NewManager(nil)
	if err != nil {
		t.Fatal(err)
	}
	first.SetTransports(registry)
	req := nodeSessionRequest("open")
	binding := harness.NodeSessionContext{Authority: req.Authority, Binding: req.Binding, CommandID: "wire-input"}
	ctx, cancel := context.WithTimeout(harness.WithNodeSession(t.Context(), binding), 10*time.Second)
	defer cancel()
	workspace := t.TempDir()
	runner, err := first.OpenSession(ctx, harness.Placement{Node: "worker", Harness: "mock"}, "", workspace, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	asked := make(chan string, 1)
	firstResult := make(chan error, 1)
	go func() {
		_, _, err := runner.(harness.TurnRunner).PromptTurn(ctx, "askme", nil, nil, func(ctx context.Context, q view.Question) (view.Answer, error) {
			asked <- q.RequestID
			<-ctx.Done()
			return view.Answer{}, ctx.Err()
		}, nil)
		firstResult <- err
	}()
	var questionID string
	select {
	case questionID = <-asked:
	case <-time.After(5 * time.Second):
		t.Fatal("agent did not ask")
	}
	first.Stop()
	registry.Close()
	cancel()
	select {
	case err := <-firstResult:
		if !errors.Is(err, acphost.ErrStopUnconfirmed) {
			t.Fatalf("detach claimed stop: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("observer did not detach")
	}
	authority.mu.Lock()
	authority.epoch = 2
	authority.writer = 2
	authority.mu.Unlock()
	binding.Authority.CoordinatorEpoch = 2
	binding.Authority.WriterGeneration = 2
	binding.Authority.CoordinatorNodeID = "hub-b"
	nextRegistry := NewRegistry("cluster-1", map[string]Config{"worker": {Addr: server.Addr(), Token: "session-test"}})
	nextRegistry.SetSessionAuthorizer(verify)
	defer nextRegistry.Close()
	next, _ := harness.NewManager(nil)
	next.SetTransports(nextRegistry)
	defer next.Stop()
	nextCtx, stop := context.WithTimeout(harness.WithNodeSession(t.Context(), binding), 10*time.Second)
	defer stop()
	attached, err := next.OpenSession(nextCtx, harness.Placement{Node: "worker", Harness: "mock"}, runner.ID(), workspace, nil)
	if err != nil {
		t.Fatal(err)
	}
	if attached.ID() != runner.ID() {
		t.Fatal("reattach created another native session")
	}
	output, _, err := attached.(harness.ResumableRunner).ResumeTurn(nextCtx, nil, func(_ context.Context, q view.Question) (view.Answer, error) {
		if q.RequestID != questionID {
			t.Error("question identity changed")
		}
		return view.Answer{Value: "Blue"}, nil
	}, nil)
	if err != nil || !strings.Contains(output, "accept:Blue") {
		t.Fatalf("reattach result %q: %v", output, err)
	}
	stale := req
	stale.Action, stale.ID = "attach", runner.ID()
	if _, err := nextRegistry.NodeSession(nextCtx, "worker", stale); err == nil {
		t.Fatal("old coordinator activation could still observe the retained execution")
	}
	if standalone {
		// Even a delayed affirmative authorization cannot undo the newer
		// coordinator activation already recorded by this native session.
		nextRegistry.SetSessionAuthorizer(func(context.Context, string, nodewire.SessionAuthority, nodewire.SessionBinding, string) error {
			return nil
		})
		if _, err := nextRegistry.NodeSession(nextCtx, "worker", stale); err == nil {
			t.Fatal("delayed grant bypassed durable coordinator generation fencing")
		}
		nextRegistry.SetSessionAuthorizer(verify)
	}
	if err := next.CloseSession(nextCtx, harness.Placement{Node: "worker", Harness: "mock"}, attached.ID()); err != nil {
		t.Fatal(err)
	}
}

func TestNodeSessionCancelDistinguishesPromptSettlementAndProcessExit(t *testing.T) {
	authority := &sessionAuthorityTest{epoch: 1, writer: 1}
	s := NewServer(ServerConfig{Name: "worker", StateDir: t.TempDir(), WorkspaceRoot: t.TempDir(), Harnesses: map[string]HarnessSpec{"mock": {Command: buildMockAgent(t)}}, SessionAuthorizer: authority})
	if err := s.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer s.sessions.Close()
	req := nodeSessionRequest("open")
	req.CommandID = "open-cancel"
	req.Harness = "mock"
	req.Workdir = t.TempDir()
	state, err := s.sessions.Do(t.Context(), "cluster-1", req)
	if err != nil {
		t.Fatal(err)
	}
	req.ID = state.ID
	req.Action = "prompt"
	req.CommandID = "slow-cancel"
	req.InputSequence = 1
	req.Text = "slow"
	state, err = s.sessions.Do(t.Context(), "cluster-1", req)
	if err != nil {
		t.Fatal(err)
	}
	cancelCtx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	req.Action = "cancel"
	state, err = s.sessions.Do(cancelCtx, "cluster-1", req)
	if err != nil || state.Command == nil || !state.Command.Settled || state.Command.State != "cancelled" || state.ProcessStopped {
		t.Fatalf("cancel receipt: %+v %v", state, err)
	}
	req.Action = "abort"
	state, err = s.sessions.Do(t.Context(), "cluster-1", req)
	if err != nil || !state.ProcessStopped {
		t.Fatalf("abort did not prove process exit: %+v %v", state, err)
	}
}

func TestNodeSessionAbortKeepsTerminalStateAfterPromptCallbackReturns(t *testing.T) {
	authority := &sessionAuthorityTest{epoch: 1, writer: 1}
	s := NewServer(ServerConfig{Name: "worker", StateDir: t.TempDir(), WorkspaceRoot: t.TempDir(), Harnesses: map[string]HarnessSpec{"mock": {Command: buildMockAgent(t)}}, SessionAuthorizer: authority})
	if err := s.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer s.sessions.Close()
	req := nodeSessionRequest("open")
	req.CommandID = "open-abort"
	req.Harness = "mock"
	req.Workdir = t.TempDir()
	state, err := s.sessions.Do(t.Context(), "cluster-1", req)
	if err != nil {
		t.Fatal(err)
	}
	req.ID = state.ID
	req.Action = "prompt"
	req.CommandID = "input-abort"
	req.InputSequence = 1
	req.Text = "askme"
	state, err = s.sessions.Do(t.Context(), "cluster-1", req)
	if err != nil {
		t.Fatal(err)
	}
	for len(state.Questions) == 0 {
		poll := req
		poll.Action = "poll"
		poll.After = state.Sequence
		poll.WaitMS = 50
		state, err = s.sessions.Do(t.Context(), "cluster-1", poll)
		if err != nil {
			t.Fatal(err)
		}
	}
	req.Action = "abort"
	if _, err := s.sessions.Do(t.Context(), "cluster-1", req); err != nil {
		t.Fatal(err)
	}
	s.sessions.wg.Wait()
	req.Action = "attach"
	state, err = s.sessions.Do(t.Context(), "cluster-1", req)
	if err != nil || state.State != "closed" || !state.ProcessStopped {
		t.Fatalf("late callback reopened closed session: %+v %v", state, err)
	}
}

func TestNodeSessionCancelUnblocksTheCoordinatorQuestionWaiter(t *testing.T) {
	authority := &sessionAuthorityTest{epoch: 1, writer: 1}
	server := startNode(t, ServerConfig{Name: "worker", Token: "question-cancel", StateDir: t.TempDir(), WorkspaceRoot: t.TempDir(), Harnesses: map[string]HarnessSpec{"mock": {Command: buildMockAgent(t)}}, SessionAuthorizer: authority})
	registry := NewRegistry("cluster-1", map[string]Config{"worker": {Addr: server.Addr(), Token: "question-cancel"}})
	defer registry.Close()
	manager, _ := harness.NewManager(nil)
	manager.SetTransports(registry)
	defer manager.Stop()
	request := nodeSessionRequest("open")
	ctx, cancel := context.WithTimeout(harness.WithNodeSession(t.Context(), harness.NodeSessionContext{Authority: request.Authority, Binding: request.Binding, CommandID: "question-cancel"}), 10*time.Second)
	defer cancel()
	runner, err := manager.OpenSession(ctx, harness.Placement{Node: "worker", Harness: "mock"}, "", t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	asked := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, _, err := runner.(harness.TurnRunner).PromptTurn(ctx, "askme", nil, nil, func(ctx context.Context, _ view.Question) (view.Answer, error) {
			close(asked)
			<-ctx.Done()
			return view.Answer{}, ctx.Err()
		}, nil)
		done <- err
	}()
	select {
	case <-asked:
	case <-time.After(5 * time.Second):
		t.Fatal("question did not reach coordinator")
	}
	if err := runner.Cancel(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if errors.Is(err, harness.ErrStopUnconfirmed) {
			t.Fatalf("settled cancellation was left pending: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("node cancellation did not release coordinator question callback")
	}
}
