package node

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/gopact-ai/steve/internal/nodewire"
)

func TestNodeReceiptRPCRequiresExactLiveHubProof(t *testing.T) {
	one, request, _ := ackFixture(t)
	dir := one.service.server.conf().StateDir
	one.service.closeRecords()
	server := startNode(t, ServerConfig{Name: "worker", Token: "receipt-test", StateDir: dir, SessionAuthorizer: CoordinatorSessionAuthorizer{}})
	registry := NewRegistry("cluster-1", map[string]Config{"worker": {Addr: server.Addr(), Token: "receipt-test"}})
	defer registry.Close()
	if err := registry.AcknowledgeNodeReceipt(t.Context(), "worker", request); err == nil {
		t.Fatal("ordinary session authority or client claim deleted a receipt")
	}
	calls := 0
	registry.SetNodeReceiptAuthorizer(func(_ context.Context, node string, authority nodewire.SessionAuthority, receipt nodewire.SessionReceipt) error {
		calls++
		if node != "worker" || authority != request.Authority || receipt != request.Receipt {
			return errors.New("proof key differs")
		}
		return nil
	})
	for range 2 {
		if err := registry.AcknowledgeNodeReceipt(t.Context(), "worker", request); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatalf("retry did not authenticate exact proof again: %d", calls)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "node-sessions", "sessions.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT command_count FROM sessions WHERE id=?`, request.Receipt.SessionID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("authenticated RPC did not delete durable input: %d %v", count, err)
	}
}

type unrelatedReceiptChallenge struct{ CoordinatorSessionAuthorizer }

func (a unrelatedReceiptChallenge) AuthorizeNodeReceipt(ctx context.Context, principal string, authority nodewire.SessionAuthority, receipt nodewire.SessionReceipt) error {
	receipt.InputSequence++
	return a.CoordinatorSessionAuthorizer.AuthorizeNodeReceipt(ctx, principal, authority, receipt)
}

func TestNodeReceiptRPCRejectsWorkerSubstitutedChallenge(t *testing.T) {
	server := startNode(t, ServerConfig{Name: "worker", Token: "receipt-test", StateDir: t.TempDir(), SessionAuthorizer: unrelatedReceiptChallenge{}})
	_, request, _ := ackFixture(t)
	registry := NewRegistry("cluster-1", map[string]Config{"worker": {Addr: server.Addr(), Token: "receipt-test"}})
	defer registry.Close()
	calls := 0
	registry.SetNodeReceiptAuthorizer(func(context.Context, string, nodewire.SessionAuthority, nodewire.SessionReceipt) error {
		calls++
		return nil
	})
	if err := registry.AcknowledgeNodeReceipt(t.Context(), "worker", request); err == nil || calls != 0 {
		t.Fatalf("worker changed the original proof key: calls=%d err=%v", calls, err)
	}
}
