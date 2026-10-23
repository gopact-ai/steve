package node

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/nodewire"
)

func TestSessionDispatchPreservesObservationAndRejectsClientStart(t *testing.T) {
	server := NewServer(ServerConfig{Name: "worker", StateDir: t.TempDir(), SessionAuthorizer: &sessionAuthorityTest{epoch: 1, writer: 1}})
	if err := server.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer server.sessions.Close()
	req := nodeSessionRequest(nodewire.SessionActionOpen)
	req.ID = "ns_" + strings.Repeat("a", 64)
	state := nodewire.SessionState{ID: req.ID, Binding: req.Binding, State: nodewire.SessionIdle, Sequence: 7}
	server.sessions.sessions[req.ID] = &ownedSession{
		service:           server.sessions,
		changed:           make(chan struct{}),
		processConfigHash: processConfigHash(req),
		record: sessionRecord{
			Format:     1,
			ClusterID:  req.Authority.ClusterID,
			Authority:  req.Authority,
			ConfigHash: sessionConfigHash(req),
			State:      state,
		},
	}
	store, err := server.sessions.recordsStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.save(sessionRecord{}, server.sessions.sessions[req.ID].record); err != nil {
		t.Fatal(err)
	}
	for _, action := range []nodewire.SessionAction{nodewire.SessionActionOpen, nodewire.SessionActionAttach, nodewire.SessionActionSettings, nodewire.SessionActionPoll} {
		t.Run(string(action), func(t *testing.T) {
			req.Action = action
			got, err := server.sessions.Do(t.Context(), "cluster-1", req)
			if err != nil || got.ID != state.ID || got.State != state.State || got.Sequence != state.Sequence {
				t.Fatalf("observation changed session: %+v, %v", got, err)
			}
		})
	}
	for _, action := range []nodewire.SessionAction{nodewire.SessionActionStart, "future-action", ""} {
		t.Run(string(action), func(t *testing.T) {
			req.Action = action
			_, err := server.sessions.Do(t.Context(), "cluster-1", req)
			var classified *SessionError
			if !errors.As(err, &classified) || classified.Code != "invalid" || classified.Message != "unknown node session action" {
				t.Fatalf("client action must be rejected: %v", err)
			}
		})
	}
}

// An owner who stops approving each command means this turn, not the next
// one: the session takes the change while it is answering, keeps its
// running receipt, and the agent reports the revised selector.
func TestARunningSessionTakesASettingsChange(t *testing.T) {
	server := NewServer(ServerConfig{Name: "worker", StateDir: t.TempDir(), WorkspaceRoot: t.TempDir(), Harnesses: map[string]HarnessSpec{"mock": {Command: buildMockAgent(t)}}, SessionAuthorizer: &sessionAuthorityTest{epoch: 1, writer: 1}})
	if err := server.startSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer server.sessions.Close()
	req := nodeSessionRequest(nodewire.SessionActionOpen)
	req.Harness, req.CommandID, req.Workdir = "mock", "open-live-option", t.TempDir()
	opened, err := server.sessions.Do(t.Context(), "cluster-1", req)
	if err != nil {
		t.Fatal(err)
	}
	req.ID = opened.ID
	req.Action, req.CommandID, req.InputSequence, req.Text = nodewire.SessionActionPrompt, "slow-turn", 1, "slow"
	running, err := server.sessions.Do(t.Context(), "cluster-1", req)
	if err != nil || running.Command == nil || running.Command.Settled {
		t.Fatalf("prompt did not stay in flight: %+v %v", running, err)
	}
	option := req
	option.Action, option.Text, option.InputSequence = nodewire.SessionActionOption, "", 0
	option.OptionID, option.OptionValue = "mode", "read-only"
	changed, err := server.sessions.Do(t.Context(), "cluster-1", option)
	if err != nil {
		t.Fatalf("running session refused a settings change: %v", err)
	}
	if changed.Settings.Mode != "Read-only" {
		t.Fatalf("agent did not report the revised mode: %+v", changed.Settings)
	}
	if changed.State != nodewire.SessionRunning || changed.Command == nil || changed.Command.Settled {
		t.Fatalf("settings change disturbed the turn: %+v", changed)
	}
	cancelCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req.Action = nodewire.SessionActionCancel
	if settled, err := server.sessions.Do(cancelCtx, "cluster-1", req); err != nil || settled.Command == nil || !settled.Command.Settled {
		t.Fatalf("turn did not settle after the change: %+v %v", settled, err)
	}
}
