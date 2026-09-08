package node

import (
	"errors"
	"strings"
	"testing"

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
	state := nodewire.SessionState{ID: req.ID, Binding: req.Binding, State: "idle", Sequence: 7}
	server.sessions.sessions[req.ID] = &ownedSession{
		service: server.sessions,
		changed: make(chan struct{}),
		record: sessionRecord{
			ClusterID:  req.Authority.ClusterID,
			Authority:  req.Authority,
			ConfigHash: sessionConfigHash(req),
			State:      state,
		},
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
