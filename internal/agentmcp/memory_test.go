package agentmcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/gopact-ai/steve/internal/memory"
)

type testMemorizer struct {
	Memorizer
	svc *memory.Service
}

func (m testMemorizer) Remember(ctx context.Context, conversation, agent, delegatedBy, rawScope, section, text, key string) (memory.Receipt, memory.Scope, error) {
	scope, err := memory.ParseScope(rawScope, "test")
	if err != nil {
		return memory.Receipt{}, scope, err
	}
	r, err := m.svc.Remember(ctx, scope, section, text, key, memory.Actor{Conversation: conversation, Agent: agent, By: "agent"})
	return r, scope, err
}

func TestRememberToolReplaysReceipt(t *testing.T) {
	dir := t.TempDir()
	svc := memory.NewService(memory.NewMarkdown("", dir), filepath.Join(dir, "audit.jsonl"))
	s := &Server{}
	s.SetMemorizer(testMemorizer{svc: svc})
	bind := binding{conversationID: "chat", agentID: "codex"}
	first, err := s.steveRemember(t.Context(), bind, json.RawMessage(`{"scope":"project","text":"first intent","idempotency_key":"retry"}`))
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.steveRemember(t.Context(), bind, json.RawMessage(`{"scope":"project","text":"changed retry","idempotency_key":"retry"}`))
	if err != nil || got != first {
		t.Fatalf("tool receipt changed on retry: %s, %v; want %s", got, err, first)
	}
	// The new argument stays optional for existing agents.
	if _, err := s.steveRemember(t.Context(), bind, json.RawMessage(`{"scope":"project","text":"first intent"}`)); err != nil {
		t.Fatal(err)
	}
	items, err := svc.List(t.Context(), memory.ProjectScope("test"))
	if err != nil || len(items) != 1 {
		t.Fatalf("tool wrote more than one fact: %+v, %v", items, err)
	}
}
