package node

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/gopact-ai/steve/internal/nodewire"
)

func waitRecordedInput(t *testing.T, s *SessionService, req nodewire.SessionRequest) nodewire.SessionState {
	t.Helper()
	req.Action = nodewire.SessionActionPoll
	req.WaitMS = 100
	for range 150 {
		state, err := s.Do(t.Context(), "cluster-1", req)
		if err != nil {
			t.Fatal(err)
		}
		if state.Command != nil && state.Command.Settled {
			return state
		}
		req.After = state.Sequence
	}
	t.Fatal("mock input did not settle")
	return nodewire.SessionState{}
}

func TestNodeInputHintsAreSingleUsePerOriginalBinding(t *testing.T) {
	authority := &sessionAuthorityTest{epoch: 1, writer: 1}
	authorize := sessionAuthorizerFunc(func(ctx context.Context, principal string, a nodewire.SessionAuthority, b nodewire.SessionBinding, action nodewire.SessionAction) error {
		switch b.AttemptID {
		case "attempt-1", "second-attempt", "third-attempt", "fourth-attempt":
			b.AttemptID = "attempt-1"
			return authority.AuthorizeNodeSession(ctx, principal, a, b, action)
		default:
			return errors.New("unadmitted test execution")
		}
	})
	cfg := ServerConfig{Name: "worker", StateDir: t.TempDir(), WorkspaceRoot: t.TempDir(),
		Harnesses:         map[string]HarnessSpec{"mock": {Command: buildMockAgent(t)}},
		SessionAuthorizer: authorize}
	s := NewServer(cfg)
	if err := s.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer s.sessions.Close()
	open := nodeSessionRequest(nodewire.SessionActionOpen)
	open.Harness, open.Workdir, open.CommandID = "mock", t.TempDir(), "first/open"
	first, err := s.sessions.Do(t.Context(), "cluster-1", open)
	if err != nil {
		t.Fatal(err)
	}
	input := open
	input.ID, input.Action, input.CommandID = first.ID, nodewire.SessionActionAttach, "first-input"
	hint, err := s.sessions.Do(t.Context(), "cluster-1", input)
	if err != nil || hint.NextInputSequence != 1 || hint.Binding != input.Binding {
		t.Fatalf("first binding has no exact hint: %+v %v", hint, err)
	}
	input.Action, input.InputSequence, input.Text = nodewire.SessionActionPrompt, hint.NextInputSequence, "first"
	if _, err := s.sessions.Do(t.Context(), "cluster-1", input); err != nil {
		t.Fatal(err)
	}
	waitRecordedInput(t, s.sessions, input)
	extra := input
	extra.Action, extra.CommandID, extra.InputSequence = nodewire.SessionActionAttach, "different-input", 0
	if _, err := s.sessions.Do(t.Context(), "cluster-1", extra); err == nil {
		t.Fatal("consumed binding minted another sequence from missing receipt")
	}
	extra.Action, extra.InputSequence = nodewire.SessionActionPrompt, 2
	if _, err := s.sessions.Do(t.Context(), "cluster-1", extra); err == nil {
		t.Fatal("explicit next sequence bypassed one-input-per-binding")
	}

	warm := open
	warm.ID, warm.CommandID = first.ID, "second/open"
	warm.Binding.AttemptID, warm.Binding.TaskID = "second-attempt", "second-task"
	if _, err := s.sessions.Do(t.Context(), "cluster-1", warm); err != nil {
		t.Fatal(err)
	}
	second := warm
	second.Action, second.CommandID = nodewire.SessionActionAttach, "second-input"
	hint, err = s.sessions.Do(t.Context(), "cluster-1", second)
	if err != nil || hint.NextInputSequence != 2 || hint.Binding != warm.Binding {
		t.Fatalf("warm rebind lost its first legal input: %+v %v", hint, err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var winners []nodewire.SessionRequest
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := second
			req.Action, req.CommandID, req.InputSequence, req.Text = nodewire.SessionActionPrompt, fmt.Sprintf("second-%d", i), hint.NextInputSequence, "second"
			if _, err := s.sessions.Do(t.Context(), "cluster-1", req); err == nil {
				mu.Lock()
				winners = append(winners, req)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(winners) != 1 {
		t.Fatalf("one hint admitted %d concurrent inputs", len(winners))
	}
	waitRecordedInput(t, s.sessions, winners[0])
	old := input
	old.Action = nodewire.SessionActionAttach
	receipt, err := s.sessions.Do(t.Context(), "cluster-1", old)
	if err != nil || receipt.Binding != input.Binding || receipt.Command.InputSequence != 1 || receipt.NextInputSequence != 0 {
		t.Fatalf("old binding observation lost original ownership: %+v %v", receipt, err)
	}
	if _, err := s.sessions.Do(t.Context(), "cluster-1", input); err == nil {
		t.Fatal("old binding could execute after rebind")
	}

	// A hint is not a reservation or grant that survives a different rebind.
	third := warm
	third.Binding.AttemptID, third.CommandID = "third-attempt", "third/open"
	if _, err := s.sessions.Do(t.Context(), "cluster-1", third); err != nil {
		t.Fatal(err)
	}
	stale := third
	stale.Action, stale.CommandID = nodewire.SessionActionAttach, "third-input"
	hint, err = s.sessions.Do(t.Context(), "cluster-1", stale)
	if err != nil || hint.NextInputSequence != 3 {
		t.Fatalf("third hint: %+v %v", hint, err)
	}
	fourth := third
	fourth.Binding.AttemptID, fourth.CommandID = "fourth-attempt", "fourth/open"
	if _, err := s.sessions.Do(t.Context(), "cluster-1", fourth); err != nil {
		t.Fatal(err)
	}
	stale.Action, stale.InputSequence = nodewire.SessionActionPrompt, hint.NextInputSequence
	if _, err := s.sessions.Do(t.Context(), "cluster-1", stale); err == nil {
		t.Fatal("stale hint authorized input after rebind")
	}
	s.sessions.Close()
	restarted := NewServer(cfg)
	if err := restarted.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer restarted.sessions.Close()
	receipt, err = restarted.sessions.Do(t.Context(), "cluster-1", old)
	if err != nil || receipt.Binding != old.Binding || receipt.Command.InputSequence != 1 || receipt.NextInputSequence != 0 {
		t.Fatalf("restart changed original receipt: %+v %v", receipt, err)
	}
	if _, err := restarted.sessions.Do(t.Context(), "cluster-1", stale); err == nil {
		t.Fatal("restart resurrected a stale hint")
	}
}

func TestNodeInputHintRacesRebindWithoutGrantingBoth(t *testing.T) {
	authority := &sessionAuthorityTest{epoch: 1, writer: 1}
	cfg := ServerConfig{Name: "worker", StateDir: t.TempDir(), WorkspaceRoot: t.TempDir(),
		Harnesses: map[string]HarnessSpec{"mock": {Command: buildMockAgent(t)}},
		SessionAuthorizer: sessionAuthorizerFunc(func(ctx context.Context, principal string, a nodewire.SessionAuthority, b nodewire.SessionBinding, action nodewire.SessionAction) error {
			if b.AttemptID == "replacement" {
				b.AttemptID = "attempt-1"
			}
			return authority.AuthorizeNodeSession(ctx, principal, a, b, action)
		})}
	server := NewServer(cfg)
	if err := server.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer server.sessions.Close()
	open := nodeSessionRequest(nodewire.SessionActionOpen)
	open.Harness, open.Workdir, open.CommandID = "mock", t.TempDir(), "race/open"
	state, err := server.sessions.Do(t.Context(), "cluster-1", open)
	if err != nil {
		t.Fatal(err)
	}
	input := open
	input.ID, input.Action, input.CommandID = state.ID, nodewire.SessionActionAttach, "race-input"
	hint, err := server.sessions.Do(t.Context(), "cluster-1", input)
	if err != nil || hint.NextInputSequence != 1 {
		t.Fatalf("hint: %+v %v", hint, err)
	}
	input.Action, input.InputSequence, input.Text = nodewire.SessionActionPrompt, hint.NextInputSequence, "cancel"
	rebind := open
	rebind.ID, rebind.CommandID, rebind.Binding.AttemptID = state.ID, "replacement/open", "replacement"
	ready, results := make(chan struct{}), make(chan error, 2)
	for _, req := range []nodewire.SessionRequest{input, rebind} {
		go func() {
			<-ready
			_, err := server.sessions.Do(t.Context(), "cluster-1", req)
			results <- err
		}()
	}
	close(ready)
	first, second := <-results, <-results
	if (first == nil) == (second == nil) {
		t.Fatalf("competing input and rebind must have one winner: %v / %v", first, second)
	}
}

func TestNodeConsumedBindingMissingReceiptRemainsExpiredAfterRestart(t *testing.T) {
	cfg := ServerConfig{Name: "worker", StateDir: t.TempDir(), WorkspaceRoot: t.TempDir(),
		Harnesses:         map[string]HarnessSpec{"mock": {Command: buildMockAgent(t)}},
		SessionAuthorizer: &sessionAuthorityTest{epoch: 1, writer: 1}}
	server := NewServer(cfg)
	if err := server.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer server.sessions.Close()
	open := nodeSessionRequest(nodewire.SessionActionOpen)
	open.Harness, open.Workdir, open.CommandID = "mock", t.TempDir(), "restart/open"
	state, err := server.sessions.Do(t.Context(), "cluster-1", open)
	if err != nil {
		t.Fatal(err)
	}
	input := open
	input.ID, input.Action, input.CommandID = state.ID, nodewire.SessionActionPrompt, "consumed"
	input.InputSequence, input.Text = 1, "one input"
	if _, err := server.sessions.Do(t.Context(), "cluster-1", input); err != nil {
		t.Fatal(err)
	}
	waitRecordedInput(t, server.sessions, input)
	server.sessions.Close()
	store, err := openSessionRecords(filepath.Join(cfg.StateDir, "node-sessions", "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	// Simulate missing evidence, not an acknowledgement endpoint in A.
	_, deleteErr := store.db.Exec(`DELETE FROM session_commands WHERE session_id=?`, state.ID)
	if err := errors.Join(deleteErr, store.close()); err != nil {
		t.Fatal(err)
	}
	restarted := NewServer(cfg)
	if err := restarted.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer restarted.sessions.Close()
	for _, seq := range []uint64{0, 1} {
		for _, action := range []nodewire.SessionAction{nodewire.SessionActionAttach, nodewire.SessionActionPrompt} {
			req := input
			req.Action, req.InputSequence = action, seq
			_, err := restarted.sessions.Do(t.Context(), "cluster-1", req)
			var refusal *SessionError
			if !errors.As(err, &refusal) || refusal.Code != "receipt_expired" {
				t.Fatalf("%s seq=%d reminted missing input after restart: %v", action, seq, err)
			}
		}
	}
}
