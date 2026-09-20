package agentmcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/capability"
)

type wrappedPreparationStore struct{ Store }

func (s wrappedPreparationStore) Update(ctx context.Context, fn func(StoreTx) error) error {
	if err := s.Store.Update(ctx, fn); err != nil {
		return fmt.Errorf("grant transaction: %w", err)
	}
	return nil
}

func TestDeniedPreparationDoesNotFenceGate(t *testing.T) {
	for _, reason := range []string{"rotated", "revoked", "different binding", "stale index"} {
		t.Run(reason, func(t *testing.T) {
			store := newGrantStore()
			scope := testScope()
			store.allowed[scope.AttemptID] = true
			original, _ := grantFixture(t, store)
			original.Extras("chat", "agent", "original-token", "")
			original.Extras("chat", "agent", "replacement-token", "")
			b := binding{conversationID: "chat", agentID: "agent"}
			deniedToken := "original-token"
			switch reason {
			case "revoked":
				original.Extras("other-chat", "agent", "revoked-token", "")
				original.Revoke("revoked-token")
				b.conversationID = "other-chat"
				deniedToken = "revoked-token"
			case "different binding":
				b.conversationID = "other-chat"
				deniedToken = "replacement-token"
			case "stale index":
				// A retained grant is not current merely because its record exists.
				store.mu.Lock()
				store.data["grant/"+digest(deniedToken)] = []byte(`{"binding":{"conversation_id":"chat","agent_id":"agent"}}`)
				store.mu.Unlock()
			}
			if err := original.BindExecution(t.Context(), Binding{ConversationID: "chat", AgentID: "agent"}, scope); err != nil {
				t.Fatal(err)
			}

			// A new activation must reject a retained stale token without
			// taking the replacement execution down with it.
			s, _ := startServer(t)
			failed := make(chan error, 1)
			if err := s.SetStore(wrappedPreparationStore{store}, func(err error) { failed <- err }); err != nil {
				t.Fatal(err)
			}
			s.mu.Lock()
			err := s.prepareLocked(b, deniedToken)
			storeErr := s.storeErr
			_, registered := s.tokens[deniedToken]
			s.mu.Unlock()
			if !errors.Is(err, ErrGrantDenied) {
				t.Errorf("preparation error = %v, want ErrGrantDenied", err)
			}
			if storeErr != nil {
				t.Errorf("authorization rejection fenced store: %v", storeErr)
			}
			if registered {
				t.Error("denied token registered in memory")
			}
			if got := rpc(t, s.URL(), "original-token", "initialize", nil); got.status != http.StatusUnauthorized {
				t.Errorf("old token reactivated: %s", got.rawBody)
			}
			if extras := s.Extras("chat", "agent", "replacement-token", ""); len(extras) != 1 {
				t.Error("valid replacement capability lost")
			}
			if got := rpc(t, s.URL(), "replacement-token", "tools/call", map[string]any{"name": "steve_help"}); got.status != http.StatusOK {
				t.Errorf("valid replacement execution refused: %s", got.rawBody)
			}
			select {
			case err := <-failed:
				t.Errorf("authorization rejection called onFailure: %v", err)
			case <-time.After(20 * time.Millisecond):
			}
		})
	}
}

func TestPrepareExtrasInputAndCompatibility(t *testing.T) {
	var absent *Server
	if extras, err := absent.PrepareExtras("chat", "agent", "token", ""); extras != nil || err != nil {
		t.Fatalf("absent server: extras=%v error=%v", extras, err)
	}
	if extras := absent.Extras("chat", "agent", "token", ""); extras != nil {
		t.Fatalf("absent server compatibility: %v", extras)
	}
	s, _ := grantFixture(t, newGrantStore())
	for _, tc := range []struct{ conversation, token string }{{"", "token"}, {"chat", ""}} {
		if extras, err := s.PrepareExtras(tc.conversation, "agent", tc.token, ""); extras != nil || err != ErrGrantDenied {
			t.Fatalf("invalid input: extras=%v error=%v", extras, err)
		}
		if extras := s.Extras(tc.conversation, "agent", tc.token, ""); extras != nil {
			t.Fatalf("invalid input compatibility: %v", extras)
		}
	}
	if extras := s.Extras("chat", "agent", "token", ""); len(extras) != 1 {
		t.Fatalf("valid compatibility: %v", extras)
	}
	s.Revoke("token")
	if extras := s.Extras("chat", "agent", "token", ""); extras != nil {
		t.Fatalf("revoked compatibility: %v", extras)
	}
	if extras, err := s.PrepareExtras("chat", "agent", "replacement", ""); err != nil || len(extras) != 1 {
		t.Fatalf("compatibility rejection fenced store: extras=%v error=%v", extras, err)
	}
}

func TestDescribeExtrasDoesNotPrepareGrant(t *testing.T) {
	store := newGrantStore()
	s, _ := grantFixture(t, store)
	if _, err := s.PrepareExtras("chat", "agent", "current", ""); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.failure = errors.New("description must not access the store")
	store.mu.Unlock()
	for _, endpoint := range []string{"", "http://127.0.0.1:12345/mcp"} {
		extras := s.DescribeExtras("candidate", endpoint)
		wantURL := endpoint
		if wantURL == "" {
			wantURL = s.URL()
		}
		if len(extras) != 1 || extras[0].Name != ServerName || extras[0].Server.URL != wantURL ||
			extras[0].Server.Type != "http" || extras[0].Server.Headers["Authorization"] != "Bearer candidate" ||
			extras[0].Instructions != Instructions {
			t.Fatal("description did not preserve the capability configuration")
		}
	}
	s.mu.Lock()
	_, registered := s.tokens["candidate"]
	current := s.byBind[binding{conversationID: "chat", agentID: "agent"}]
	storeErr := s.storeErr
	s.mu.Unlock()
	if registered || current != "current" || storeErr != nil {
		t.Fatal("description changed gate state")
	}
	store.mu.Lock()
	store.failure = nil
	store.mu.Unlock()
	if _, _, err := s.authenticate(t.Context(), "current", false); err != nil {
		t.Fatalf("description revoked the current grant: %v", err)
	}
	if _, _, err := s.authenticate(t.Context(), "candidate", false); err != ErrGrantDenied {
		t.Fatalf("description authorized an unprepared grant: %v", err)
	}
}

func TestPrepareCapabilitiesReturnsGrantDenied(t *testing.T) {
	for _, tc := range []struct {
		name    string
		binding Binding
		prepare func(*Server, string, string) ([]capability.Extra, error)
	}{
		{
			name:    "chat",
			binding: Binding{ConversationID: "chat", AgentID: "agent"},
			prepare: func(s *Server, token, endpoint string) ([]capability.Extra, error) {
				return s.PrepareExtras("chat", "agent", token, endpoint)
			},
		},
		{
			name:    "delegated",
			binding: Binding{ConversationID: "chat", AgentID: "agent", TaskID: "task-1", DelegatedBy: "parent"},
			prepare: func(s *Server, token, endpoint string) ([]capability.Extra, error) {
				return s.PrepareDelegated("chat", "agent", "task-1", "parent", token, endpoint)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newGrantStore()
			scope := testScope()
			store.allowed[scope.AttemptID] = true
			s, _ := grantFixture(t, store)
			if extras, err := tc.prepare(s, "token", ""); err != nil || len(extras) != 1 || extras[0].Server.URL != s.URL() {
				t.Fatalf("initial preparation: extras=%v error=%v", extras, err)
			}
			if err := s.BindExecution(t.Context(), tc.binding, scope); err != nil {
				t.Fatal(err)
			}
			fresh, _ := grantFixture(t, store)
			endpoint := "http://127.0.0.1:12345/mcp"
			extras, err := tc.prepare(fresh, "token", endpoint)
			if err != nil || len(extras) != 1 {
				t.Fatalf("same-token restart: extras=%v error=%v", extras, err)
			}
			if extras[0].Name != ServerName || extras[0].Instructions != Instructions || extras[0].Server.Type != "http" || extras[0].Server.URL != endpoint || extras[0].Server.Headers["Authorization"] != "Bearer token" {
				t.Fatalf("capability changed: %+v", extras[0])
			}
			if _, _, err := fresh.authenticate(t.Context(), "token", true); err != nil {
				t.Fatalf("same-token restart lost execution: %v", err)
			}
			fresh.Revoke("token")
			if extras, err := tc.prepare(fresh, "token", ""); err != ErrGrantDenied || extras != nil {
				t.Fatalf("revoked preparation: extras=%v error=%v, want nil, ErrGrantDenied", extras, err)
			}
			if extras, err := tc.prepare(fresh, "replacement", ""); err != nil || len(extras) != 1 {
				t.Fatalf("replacement preparation: extras=%v error=%v", extras, err)
			}
			if _, _, err := fresh.authenticate(t.Context(), "token", false); err != ErrGrantDenied {
				t.Fatalf("revoked token reactivated: %v", err)
			}
			if err := fresh.BindExecution(t.Context(), tc.binding, scope); err != nil {
				t.Fatalf("replacement bind: %v", err)
			}
			if _, _, err := fresh.authenticate(t.Context(), "replacement", true); err != nil {
				t.Fatalf("replacement execution: %v", err)
			}
		})
	}
}

func TestPrepareExtrasStoreFailureStillFences(t *testing.T) {
	store := newGrantStore()
	s, _ := startServer(t)
	failed := make(chan error, 2)
	if err := s.SetStore(store, func(err error) { failed <- err }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PrepareExtras("chat", "agent", "token", ""); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("replicated commit failed")
	store.mu.Lock()
	store.failure = failure
	store.mu.Unlock()
	extras, err := s.PrepareExtras("chat", "agent", "replacement", "")
	if extras != nil || !errors.Is(err, failure) || errors.Is(err, ErrGrantDenied) {
		t.Fatalf("store failure: extras=%v error=%v", extras, err)
	}
	latched := err
	select {
	case err := <-failed:
		if err != latched {
			t.Fatalf("onFailure error = %v, want latched error %v", err, latched)
		}
	case <-time.After(time.Second):
		t.Fatal("store failure did not call onFailure")
	}
	store.mu.Lock()
	store.failure = nil
	store.mu.Unlock()
	if extras, err := s.PrepareExtras("chat", "agent", "replacement", ""); extras != nil || err != latched {
		t.Fatalf("failed gate revived: extras=%v error=%v", extras, err)
	}
	if _, _, err := s.authenticate(t.Context(), "token", false); err != latched {
		t.Fatalf("existing grant escaped fence: %v", err)
	}
	s.mu.Lock()
	_, registered := s.tokens["replacement"]
	storeErr := s.storeErr
	s.mu.Unlock()
	if registered || storeErr != latched {
		t.Fatalf("failed preparation changed memory: registered=%v storeErr=%v", registered, storeErr)
	}
	select {
	case err := <-failed:
		t.Fatalf("onFailure called more than once: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
}
