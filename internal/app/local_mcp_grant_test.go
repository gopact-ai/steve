package app

import (
	"errors"
	"net/http"
	"testing"

	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/attempt"
)

// A local ACP process has an opaque native ID, not an authenticated node ID.
// Its tools still require the original running attempt and this hub activation;
// another activation cannot make that same old execution live again.
func TestLocalMCPGrantPinsNativeSessionAndHubActivation(t *testing.T) {
	book, tasks := applicationGrantBook(t)
	gate := applicationGrantGate(t, book, true)
	gate.Extras("chat", "agent", "local-native-token", "")
	record, scope := openGrantAttemptOn(t, book, tasks, "local-original", "ns_prepared", "")
	attempts := attempt.New(book)
	nativeID := "opaque-upstream-session"
	if _, err := attempts.Advance(t.Context(), record.ID, attempt.Running, "local prompt started", func(r *attempt.Record) { r.Session = nativeID }); err != nil {
		t.Fatal(err)
	}
	scope.SessionID = nativeID
	binding := agentmcp.Binding{ConversationID: "chat", AgentID: "agent"}
	forged := scope
	forged.SessionID = "a different native context"
	if err := gate.BindExecution(t.Context(), binding, forged); !errors.Is(err, agentmcp.ErrGrantDenied) {
		t.Fatalf("different native process admitted: %v", err)
	}
	if err := gate.BindExecution(t.Context(), binding, scope); err != nil {
		t.Fatalf("live local process refused: %v", err)
	}
	if status := grantRPC(t, gate, "local-native-token", "tools/call"); status != http.StatusOK {
		t.Fatalf("live local tools: %d", status)
	}
	fresh := applicationGrantGate(t, book, true)
	fresh.Extras("chat", "agent", "local-native-token", "")
	if status := grantRPC(t, fresh, "local-native-token", "tools/call"); status != http.StatusUnauthorized {
		t.Fatalf("old local activation retained tools: %d", status)
	}
	if status := grantRPC(t, gate, "local-native-token", "tools/call"); status != http.StatusUnauthorized {
		t.Fatalf("old process bypassed activation fence: %d", status)
	}
	if err := attempts.MarkSessionSettled(t.Context(), record.ID, "original prompt settled"); err != nil {
		t.Fatal(err)
	}
	if _, err := attempts.Fail(t.Context(), record.ID, "ended", "native prompt ended"); err != nil {
		t.Fatal(err)
	}
	next, nextScope := openGrantAttemptOn(t, book, tasks, "local-next", "ns_next", "")
	if _, err := attempts.Advance(t.Context(), next.ID, attempt.Running, "next local prompt", func(r *attempt.Record) { r.Session = nativeID }); err != nil {
		t.Fatal(err)
	}
	nextScope.SessionID = nativeID
	if err := fresh.BindExecution(t.Context(), binding, nextScope); err != nil {
		t.Fatalf("settled history could not continue under fresh activation: %v", err)
	}
	if status := grantRPC(t, fresh, "local-native-token", "tools/call"); status != http.StatusOK {
		t.Fatalf("fresh local tools: %d", status)
	}
}
