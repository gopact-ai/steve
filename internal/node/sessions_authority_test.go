package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/nodewire"
)

type sessionAuthorizerFunc func(context.Context, string, nodewire.SessionAuthority, nodewire.SessionBinding, nodewire.SessionAction) error

func (f sessionAuthorizerFunc) AuthorizeNodeSession(ctx context.Context, principal string, a nodewire.SessionAuthority, b nodewire.SessionBinding, action nodewire.SessionAction) error {
	return f(ctx, principal, a, b, action)
}

func TestSessionAuthorityChallengeCannotAuthorizeUnrelatedActions(t *testing.T) {
	for _, mode := range []string{"unrelated-action", "too-many-challenges"} {
		t.Run(mode, func(t *testing.T) {
			delegate := CoordinatorSessionAuthorizer{}
			verifier := sessionAuthorizerFunc(func(ctx context.Context, principal string, a nodewire.SessionAuthority, b nodewire.SessionBinding, action nodewire.SessionAction) error {
				if mode == "unrelated-action" {
					return delegate.AuthorizeNodeSession(ctx, principal, a, b, "abort")
				}
				for range 3 {
					if err := delegate.AuthorizeNodeSession(ctx, principal, a, b, action); err != nil {
						return err
					}
				}
				return nil
			})
			server := startNode(t, ServerConfig{Name: "worker", Token: "challenge-test", StateDir: t.TempDir(), SessionAuthorizer: verifier})
			registry := NewRegistry("cluster-1", map[string]Config{"worker": {Addr: server.Addr(), Token: "challenge-test"}})
			defer registry.Close()
			calls := 0
			registry.SetSessionAuthorizer(func(_ context.Context, _ string, _ nodewire.SessionAuthority, _ nodewire.SessionBinding, action nodewire.SessionAction) error {
				calls++
				if action != "capabilities" {
					t.Error("unrelated action reached application authorizer")
				}
				return nil
			})
			if _, err := registry.NodeSession(t.Context(), "worker", nodeSessionRequest("capabilities")); err == nil {
				t.Fatal("invalid authority challenge succeeded")
			}
			if (mode == "unrelated-action" && calls != 0) || calls > 2 {
				t.Fatalf("invalid challenge reached coordinator %d times", calls)
			}
		})
	}
}

func TestStandaloneSessionRequiresCoordinatorAuthorityAndFreshStartAdmission(t *testing.T) {
	for _, mode := range []string{"missing-verifier", "expired-start", "wrong-cluster", "lost-connection"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			server := startNode(t, ServerConfig{Name: "worker", Token: "authority-test", StateDir: root, Harnesses: map[string]HarnessSpec{"mock": {Command: "/never-start"}}, SessionAuthorizer: CoordinatorSessionAuthorizer{}})
			registry := NewRegistry("cluster-1", map[string]Config{"worker": {Addr: server.Addr(), Token: "authority-test"}})
			defer registry.Close()
			calls := []nodewire.SessionAction{}
			if mode != "missing-verifier" {
				registry.SetSessionAuthorizer(func(_ context.Context, node string, a nodewire.SessionAuthority, b nodewire.SessionBinding, action nodewire.SessionAction) error {
					calls = append(calls, action)
					if node != "worker" || b.NodeID != node || a.ClusterID != "cluster-1" {
						return errors.New("incorrect identity")
					}
					if mode == "expired-start" && action == "start" {
						return errors.New("execution lease expired")
					}
					if mode == "lost-connection" {
						registry.Close()
					}
					return nil
				})
			}
			req := nodeSessionRequest("open")
			req.Harness, req.Workdir, req.CommandID = "mock", t.TempDir(), "authority-open"
			if mode == "wrong-cluster" {
				req.Authority.ClusterID = "other-cluster"
			}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			if _, err := registry.NodeSession(ctx, "worker", req); err == nil {
				t.Fatal("unverified authority opened a native session")
			}
			if mode == "expired-start" && (len(calls) != 2 || calls[0] != "open" || calls[1] != "start") {
				t.Fatalf("start did not require separate lease admission: %v", calls)
			}
			if mode == "wrong-cluster" && len(calls) != 0 {
				t.Fatal("unrelated cluster reached coordinator verification")
			}
			entries, err := os.ReadDir(filepath.Join(root, "node-sessions"))
			if err != nil || len(entries) != 0 {
				t.Fatalf("rejected authority created durable native execution: %v %v", entries, err)
			}
		})
	}
}
