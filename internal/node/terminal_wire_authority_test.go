package node

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/nodewire"
)

func TestStandaloneTerminalAdmissionConsumesOnlyAfterBothFreshChallenges(t *testing.T) {
	for _, mode := range []string{"allow", "second-denied", "unadvertised"} {
		t.Run(mode, func(t *testing.T) {
			one, req, count := readyTerminalAdmission(t)
			ready := make(chan *SessionService, 1)
			server := startNode(t, ServerConfig{
				Name: "worker", Token: "terminal-wire-fixture", StateDir: t.TempDir(),
			}, func(s *Server) {
				cfg := s.conf()
				cfg.SessionAuthorizer = sessionAuthorizerFunc(func(ctx context.Context, principal string, a nodewire.SessionAuthority, b nodewire.SessionBinding, action nodewire.SessionAction) error {
					if action == nodewire.SessionActionCapabilities {
						// Serve publishes sessions before its authenticated handler
						// goroutine. This channel synchronizes fixture access, unlike
						// Addr(), which reports binding before service initialization.
						ready <- s.sessions
					}
					return (CoordinatorSessionAuthorizer{}).AuthorizeNodeSession(ctx, principal, a, b, action)
				})
				s.cfg.Store(&cfg)
			})
			registry := NewRegistry("cluster-1", map[string]Config{"worker": {Addr: server.Addr(), Token: "terminal-wire-fixture"}})
			defer registry.Close()
			// No harness is registered: the readiness request cannot start work.
			_, _ = registry.NodeSession(t.Context(), "worker", nodeSessionRequest(nodewire.SessionActionCapabilities))
			service := <-ready
			// Synthetic cleanup facts test the wire and persistence only.
			// No Host/process exists and no numeric identity is ever signalled.
			one.service = service
			store, err := service.recordsStore()
			if err != nil {
				t.Fatal(err)
			}
			if err := store.save(sessionRecord{}, one.record); err != nil {
				t.Fatal(err)
			}
			service.mu.Lock()
			service.sessions[one.record.State.ID] = one
			service.mu.Unlock()
			checks := 0
			registry.SetSessionAuthorizer(func(_ context.Context, node string, a nodewire.SessionAuthority, b nodewire.SessionBinding, action nodewire.SessionAction) error {
				checks++
				if node != req.Binding.NodeID || a != req.Authority || b != req.Binding || action != nodewire.SessionActionTerminalAdmit {
					return errors.New("terminal challenge substituted original authority")
				}
				if mode == "second-denied" && checks == 2 {
					return errors.New("original execution revoked before payload admission")
				}
				return nil
			})
			if mode == "unadvertised" {
				conn, err := registry.connect(t.Context(), "worker")
				if err != nil {
					t.Fatal(err)
				}
				advert := conn.getAdvert()
				advert.Features = nil
				conn.setAdvert(advert)
			}
			_, err = registry.NodeSession(t.Context(), "worker", req)
			one.mu.Lock()
			consumed := *count
			one.mu.Unlock()
			switch mode {
			case "allow":
				if err != nil || consumed != 1 || checks != 2 {
					t.Fatalf("original gate did not require both live challenges: count=%d checks=%d err=%v", consumed, checks, err)
				}
			case "second-denied":
				if err == nil || consumed != 0 || checks != 2 {
					t.Fatalf("revoked second challenge still opened payload: count=%d checks=%d err=%v", consumed, checks, err)
				}
			case "unadvertised":
				var beforeDispatch *nodewire.SessionNotDispatched
				if !errors.As(err, &beforeDispatch) || consumed != 0 || checks != 0 {
					t.Fatalf("legacy node received a terminal admission: count=%d checks=%d err=%v", consumed, checks, err)
				}
			}
		})
	}
}

func TestTerminalAuthorityDeadlineClosesOnlyItsOriginalRequest(t *testing.T) {
	left, right := net.Pipe()
	hub, node := nodewire.NewMux(left, true), nodewire.NewMux(right, false)
	defer hub.Close()
	defer node.Close()
	stream, err := node.Open(nodewire.OpenRequest{Kind: nodewire.StreamNodeSessions})
	if err != nil {
		t.Fatal(err)
	}
	peer, err := hub.Accept(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	defer peer.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	ctx = context.WithValue(ctx, sessionAuthorizationStreamKey{}, stream)
	done := make(chan error, 1)
	req := nodeSessionRequest(nodewire.SessionActionTerminalAdmit)
	go func() {
		done <- (CoordinatorSessionAuthorizer{}).AuthorizeNodeSession(ctx, "cluster-1", req.Authority, req.Binding, req.Action)
	}()
	var challenge nodewire.SessionReply
	if err := readSessionMessage(peer, &challenge); err != nil || challenge.AuthorizeAction != req.Action {
		stream.Close()
		<-done
		t.Fatal("terminal admission did not issue its original live-stream challenge")
	}
	// The authenticated peer deliberately sends no authorization response.
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("missing terminal authority became a successful grant")
		}
	case <-time.After(500 * time.Millisecond):
		stream.Close()
		<-done
		t.Fatal("expired terminal authority remained blocked on an unbounded live stream")
	}
	// Cancellation owns only this request, not the authenticated node connection.
	other, err := node.Open(nodewire.OpenRequest{Kind: nodewire.StreamAdvert})
	if err != nil {
		t.Fatalf("terminal admission deadline destroyed the node connection: %v", err)
	}
	received, err := hub.Accept(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	other.Close()
	received.Close()
}
