package agentmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
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

// recallRecorder answers steve_recall with one hit and keeps the limit
// it was asked for.
type recallRecorder struct {
	Memorizer
	limit int
}

func (m *recallRecorder) Recall(_ context.Context, _, _, _, _ string, limit int) ([]memory.Hit, string, error) {
	m.limit = limit
	return []memory.Hit{{Item: memory.Item{ID: "g1", Scope: memory.Global, Text: "prefers tabs"}, Score: 1}}, "markdown", nil
}

func TestRecallToolKeepsLimitWithinOneToFifty(t *testing.T) {
	for asked, want := range map[string]int{"": 10, "0": 10, "-3": 10, "7": 7, "50": 50, "51": 50, "500": 50} {
		m := &recallRecorder{}
		s := &Server{}
		s.SetMemorizer(m)
		raw := `{"query":"tabs"}`
		if asked != "" {
			raw = fmt.Sprintf(`{"query":"tabs","limit":%s}`, asked)
		}
		if _, err := s.steveRecall(t.Context(), binding{conversationID: "chat", agentID: "codex"}, json.RawMessage(raw)); err != nil {
			t.Fatal(err)
		}
		if m.limit != want {
			t.Fatalf("limit %q reached the memorizer as %d, want %d", asked, m.limit, want)
		}
	}
}

func TestRecallToolServesADelegatedTask(t *testing.T) {
	s := &Server{}
	s.SetMemorizer(&recallRecorder{})
	out, err := s.steveRecall(t.Context(), binding{conversationID: "chat", agentID: "codex", delegatedBy: "parent"}, json.RawMessage(`{"query":"tabs"}`))
	if err != nil || !strings.Contains(out, "prefers tabs") {
		t.Fatalf("delegated recall: %s, %v", out, err)
	}
}

// writeRecorder answers steve_remember and steve_forget and keeps the
// delegatedBy each was given.
type writeRecorder struct {
	Memorizer
	rememberedBy, forgotBy string
}

func (m *writeRecorder) Remember(_ context.Context, _, _, delegatedBy, _, _, _, _ string) (memory.Receipt, memory.Scope, error) {
	m.rememberedBy = delegatedBy
	return memory.Receipt{ID: "g1", New: true}, memory.Global, nil
}

func (m *writeRecorder) Forget(_ context.Context, _, _, delegatedBy, _, _ string) (memory.Item, error) {
	m.forgotBy = delegatedBy
	return memory.Item{ID: "g1", Scope: memory.Global, Text: "likes go"}, nil
}

// The coordinator refuses a delegated task's writes by delegatedBy, so
// the tools pass it on.
func TestRememberAndForgetToolsPassOnWhoDelegatedTheTask(t *testing.T) {
	m := &writeRecorder{}
	s := &Server{}
	s.SetMemorizer(m)
	bind := binding{conversationID: "chat", agentID: "codex", delegatedBy: "parent"}
	if _, err := s.steveRemember(t.Context(), bind, json.RawMessage(`{"scope":"global","text":"likes go"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.steveForget(t.Context(), bind, json.RawMessage(`{"scope":"global","id":"g1"}`)); err != nil {
		t.Fatal(err)
	}
	if m.rememberedBy != "parent" || m.forgotBy != "parent" {
		t.Fatalf("delegatedBy reached the memorizer as %q from steve_remember and %q from steve_forget, want parent", m.rememberedBy, m.forgotBy)
	}
}
