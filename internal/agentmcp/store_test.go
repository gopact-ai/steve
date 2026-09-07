package agentmcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/intent"
)

type grantStore struct {
	mu      sync.Mutex
	data    map[string]json.RawMessage
	allowed map[string]bool
	settled map[string]bool
	failure error
}

type grantTx struct {
	*grantStore
	data map[string]json.RawMessage
}

func newGrantStore() *grantStore {
	return &grantStore{data: map[string]json.RawMessage{}, allowed: map[string]bool{}, settled: map[string]bool{}}
}
func (s *grantStore) Update(ctx context.Context, fn func(StoreTx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.failure != nil {
		return s.failure
	}
	next := map[string]json.RawMessage{}
	for k, v := range s.data {
		next[k] = append(json.RawMessage{}, v...)
	}
	if err := fn(&grantTx{s, next}); err != nil {
		return err
	}
	s.data = next
	return nil
}
func (s *grantTx) Get(kind, id string, out any) (bool, error) {
	raw, ok := s.data[kind+"/"+id]
	if !ok {
		return false, nil
	}
	return true, json.Unmarshal(raw, out)
}
func (s *grantTx) Put(kind, id string, value any) error {
	raw, err := json.Marshal(value)
	if err == nil {
		s.data[kind+"/"+id] = raw
	}
	return err
}
func (s *grantTx) Delete(kind, id string) error { delete(s.data, kind+"/"+id); return nil }
func (s *grantTx) Authorize(_ Binding, scope GrantScope) error {
	if !s.allowed[scope.AttemptID] || s.settled[scope.AttemptID] {
		return ErrGrantDenied
	}
	return nil
}
func (s *grantTx) Bind(b Binding, old *GrantScope, next GrantScope) error {
	if old != nil && *old != next && (!s.settled[old.AttemptID] || b.TaskID != "" || old.SessionID != next.SessionID) {
		return ErrGrantDenied
	}
	return s.Authorize(b, next)
}

func grantFixture(t *testing.T, store *grantStore) (*Server, *fakeSender) {
	t.Helper()
	s, sender := startServer(t)
	if err := s.SetStore(store, nil); err != nil {
		t.Fatal(err)
	}
	return s, sender
}
func testScope() GrantScope {
	return GrantScope{TaskID: "task-1", TaskEpoch: 2, AttemptID: "attempt-1", ExecutionGeneration: 1, NodeID: "node-a", SessionID: "ns_original"}
}

func TestOriginalTokenResumesWithFreshGateAndFixedExecution(t *testing.T) {
	store := newGrantStore()
	scope := testScope()
	store.allowed[scope.AttemptID] = true
	s, _ := grantFixture(t, store)
	s.Extras("chat", "agent", "original-token", "")
	if got := rpc(t, s.URL(), "original-token", "initialize", nil); got.status != http.StatusOK {
		t.Fatalf("pending metadata: %s", got.rawBody)
	}
	call := map[string]any{"name": "steve_help", "arguments": map[string]any{}}
	if got := rpc(t, s.URL(), "original-token", "tools/call", call); got.status != http.StatusUnauthorized {
		t.Fatalf("pending token executed tool: %s", got.rawBody)
	}
	if err := s.BindExecution(t.Context(), Binding{ConversationID: "chat", AgentID: "agent"}, scope); err != nil {
		t.Fatal(err)
	}
	fresh, _ := grantFixture(t, store)
	if err := fresh.BindExecution(t.Context(), Binding{ConversationID: "chat", AgentID: "agent"}, scope); err != nil {
		t.Fatal(err)
	}
	if got := rpc(t, fresh.URL(), "original-token", "tools/call", call); got.status != http.StatusOK {
		t.Fatalf("original token lost: %s", got.rawBody)
	}
	store.mu.Lock()
	store.allowed[scope.AttemptID] = false
	store.allowed["replacement"] = true
	store.mu.Unlock()
	if got := rpc(t, fresh.URL(), "original-token", "tools/call", call); got.status != http.StatusUnauthorized {
		t.Fatalf("old token upgraded: %s", got.rawBody)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, raw := range store.data {
		if strings.Contains(string(raw), "original-token") {
			t.Fatal("raw bearer persisted")
		}
	}
}

func TestTokenRebindRequiresSettledSameNativeSessionAndExplicitNewScope(t *testing.T) {
	store := newGrantStore()
	scope := testScope()
	store.allowed[scope.AttemptID] = true
	store.allowed["next"] = true
	s, _ := grantFixture(t, store)
	s.Extras("chat", "agent", "original-token", "")
	b := Binding{ConversationID: "chat", AgentID: "agent"}
	if err := s.BindExecution(t.Context(), b, scope); err != nil {
		t.Fatal(err)
	}
	next := scope
	next.TaskID = "task-next"
	next.AttemptID = "next"
	if err := s.BindExecution(t.Context(), b, next); !errors.Is(err, ErrGrantDenied) {
		t.Fatalf("unsettled rebind: %v", err)
	}
	store.settled[scope.AttemptID] = true
	next.SessionID = "ns_replacement"
	if err := s.BindExecution(t.Context(), b, next); !errors.Is(err, ErrGrantDenied) {
		t.Fatalf("different session reused token: %v", err)
	}
	next.SessionID = scope.SessionID
	if err := s.BindExecution(t.Context(), b, next); err != nil {
		t.Fatalf("legitimate next user turn: %v", err)
	}
	s.Extras("chat", "agent", "replacement-token", "")
	fresh, _ := grantFixture(t, store)
	if got := rpc(t, fresh.URL(), "original-token", "initialize", nil); got.status != http.StatusUnauthorized {
		t.Fatalf("rotation did not revoke: %s", got.rawBody)
	}
	fresh.Revoke("replacement-token")
	last, _ := grantFixture(t, store)
	if got := rpc(t, last.URL(), "replacement-token", "initialize", nil); got.status != http.StatusUnauthorized {
		t.Fatalf("revoke not durable: %s", got.rawBody)
	}
}

func TestMessageReceiptAnchorAndLimitsSurviveGateReplacement(t *testing.T) {
	store := newGrantStore()
	scope := testScope()
	store.allowed[scope.AttemptID] = true
	s, _ := grantFixture(t, store)
	s.Extras("chat", "agent", "original-token", "")
	s.Anchor("chat", channel.Address{Channel: "feishu", Conversation: "chat", Message: "inbound"})
	s.SetStyle("chat", "Agent style")
	if err := s.BindExecution(t.Context(), Binding{ConversationID: "chat", AgentID: "agent"}, scope); err != nil {
		t.Fatal(err)
	}
	got := rpc(t, s.URL(), "original-token", "tools/call", map[string]any{"name": "channel_send", "arguments": map[string]any{"content": "first"}})
	result := got.body["result"].(map[string]any)
	content := result["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.HasPrefix(content, "sent message_id=") {
		t.Fatalf("send: %s", got.rawBody)
	}
	handle := strings.TrimPrefix(content, "sent message_id=")
	fresh, sender := grantFixture(t, store)
	// Console recovery registers the same original exchange again.
	fresh.Anchor("chat", channel.Address{Channel: "feishu", Conversation: "chat", Message: "inbound"})
	got = rpc(t, fresh.URL(), "original-token", "tools/call", map[string]any{"name": "channel_update", "arguments": map[string]any{"message_id": handle, "content": "continued"}})
	if strings.Contains(got.rawBody, `"isError":true`) || len(sender.patches) != 1 {
		t.Fatalf("receipt lost: %s", got.rawBody)
	}
	if !fresh.Interim("chat") {
		t.Fatal("interim state lost")
	}
	for i := 1; i < maxSendsPerTurn; i++ {
		rpc(t, fresh.URL(), "original-token", "tools/call", map[string]any{"name": "channel_send", "arguments": map[string]any{"content": "more"}})
	}
	last, _ := grantFixture(t, store)
	got = rpc(t, last.URL(), "original-token", "tools/call", map[string]any{"name": "channel_send", "arguments": map[string]any{"content": "too many"}})
	if !strings.Contains(got.rawBody, "send limit reached") {
		t.Fatalf("limit reset: %s", got.rawBody)
	}
	last.Anchor("chat", channel.Address{Channel: "feishu", Conversation: "chat", Message: "new-user-input"})
	if last.Interim("chat") {
		t.Fatal("new input kept prior turn counters")
	}
	if text, bad := callTool(t, last.URL(), "original-token", "channel_update", map[string]any{"message_id": handle, "content": "wrong turn"}); !bad {
		t.Fatalf("new turn retained old message authority: %s", text)
	}
}

func TestMessageIntentUsesOriginalAttemptInsteadOfLatestTaskAttempt(t *testing.T) {
	store := newGrantStore()
	scope := testScope()
	store.allowed[scope.AttemptID] = true
	s, _ := grantFixture(t, store)
	s.Extras("chat", "agent", "token", "")
	s.Anchor("chat", channel.Address{Channel: "feishu", Conversation: "chat", Message: "inbound"})
	service, attempts := attachEffects(t, s)
	attempts.live = "replacement-attempt"
	if err := s.BindExecution(t.Context(), Binding{ConversationID: "chat", AgentID: "agent"}, scope); err != nil {
		t.Fatal(err)
	}
	if text, bad := callTool(t, s.URL(), "token", "channel_send", map[string]any{"content": "fixed execution message"}); bad {
		t.Fatal(text)
	}
	intents, err := service.ForTask(t.Context(), scope.TaskID)
	if err != nil || len(intents) != 1 || intents[0].AttemptID != scope.AttemptID {
		t.Fatalf("message moved to newer attempt: %+v %v", intents, err)
	}
}

func TestDurableFailureFencesGateWithoutAcknowledgingMemoryOnlyState(t *testing.T) {
	store := newGrantStore()
	scope := testScope()
	store.allowed[scope.AttemptID] = true
	s, _ := startServer(t)
	failed := make(chan error, 1)
	if err := s.SetStore(store, func(err error) { failed <- err }); err != nil {
		t.Fatal(err)
	}
	s.Extras("chat", "agent", "token", "")
	if err := s.BindExecution(t.Context(), Binding{ConversationID: "chat", AgentID: "agent"}, scope); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.failure = errors.New("replicated commit failed")
	store.mu.Unlock()
	s.Anchor("chat", channel.Address{Channel: "feishu", Conversation: "chat", Message: "inbound"})
	select {
	case <-failed:
	case <-time.After(time.Second):
		t.Fatal("failed persistence did not stop activation")
	}
	store.mu.Lock()
	store.failure = nil
	store.mu.Unlock()
	if got := rpc(t, s.URL(), "token", "tools/call", map[string]any{"name": "steve_help"}); got.status == http.StatusOK {
		t.Fatalf("failed gate revived: %s", got.rawBody)
	}
	if extras := s.Extras("chat", "agent", "new-token", ""); len(extras) > 0 {
		t.Fatal("unpersisted capability escaped")
	}
}

func TestUnknownMessageOperationStaysBlockedAcrossFreshGate(t *testing.T) {
	store := newGrantStore()
	scope := testScope()
	store.allowed[scope.AttemptID] = true
	s, _ := grantFixture(t, store)
	messenger := &neutralMessenger{}
	s.BindChannel("console", messenger)
	s.Extras("chat", "agent", "token", "")
	s.Anchor("chat", channel.Address{Channel: "console", Conversation: "chat", Message: "inbound"})
	if err := s.BindExecution(t.Context(), Binding{ConversationID: "chat", AgentID: "agent"}, scope); err != nil {
		t.Fatal(err)
	}
	text, bad := callTool(t, s.URL(), "token", "channel_send", map[string]any{"content": "original"})
	if bad {
		t.Fatal(text)
	}
	id := strings.TrimPrefix(text, "sent message_id=")
	messenger.err = channel.ErrOutcomeUnknown
	if _, bad := callTool(t, s.URL(), "token", "channel_recall", map[string]any{"message_id": id}); !bad {
		t.Fatal("unknown recall acknowledged")
	}
	fresh, _ := grantFixture(t, store)
	replacement := &neutralMessenger{}
	fresh.BindChannel("console", replacement)
	text, bad = callTool(t, fresh.URL(), "token", "channel_recall", map[string]any{"message_id": id})
	if !bad || !strings.Contains(text, "blocked") || len(replacement.recalls) != 0 {
		t.Fatalf("unknown recall retried: %q calls=%d", text, len(replacement.recalls))
	}
}

func TestResolvedMessageOperationRestoresReceiptAcrossFreshGate(t *testing.T) {
	for _, tool := range []string{"channel_update", "channel_recall"} {
		for _, verdict := range []string{"new", "happened"} {
			t.Run(tool+"/"+verdict, func(t *testing.T) {
				store := newGrantStore()
				scope := testScope()
				store.allowed[scope.AttemptID] = true
				s, _ := grantFixture(t, store)
				messenger := &neutralMessenger{}
				s.BindChannel("console", messenger)
				service, attempts := attachEffects(t, s)
				s.Extras("chat", "agent", "token", "")
				s.Anchor("chat", channel.Address{Channel: "console", Conversation: "chat", Message: "inbound"})
				if err := s.BindExecution(t.Context(), Binding{ConversationID: "chat", AgentID: "agent"}, scope); err != nil {
					t.Fatal(err)
				}
				text, bad := callTool(t, s.URL(), "token", "channel_send", map[string]any{"content": "original"})
				if bad {
					t.Fatal(text)
				}
				id := strings.TrimPrefix(text, "sent message_id=")
				messenger.err = channel.ErrOutcomeUnknown
				args := map[string]any{"message_id": id}
				if tool == "channel_update" {
					args["content"] = "updated"
					args["progress"] = "2/3"
				}
				if _, bad := callTool(t, s.URL(), "token", tool, args); !bad {
					t.Fatal("unknown outcome acknowledged")
				}
				unknown, err := service.Unresolved(t.Context())
				if err != nil || len(unknown) != 1 {
					t.Fatalf("unknown intent: %+v %v", unknown, err)
				}
				if _, err := service.Resolve(t.Context(), unknown[0].ID, verdict, "owner"); err != nil {
					t.Fatal(err)
				}
				fresh, _ := grantFixture(t, store)
				replacement := &neutralMessenger{}
				fresh.BindChannel("console", replacement)
				fresh.SetIntents(intent.ForAgents{S: service, Attempts: attempts})
				fresh.Anchor("chat", channel.Address{Channel: "console", Conversation: "chat", Message: "inbound"})
				if verdict == "happened" && tool == "channel_recall" {
					if _, bad := callTool(t, fresh.URL(), "token", tool, args); !bad {
						t.Fatal("already recalled message retained")
					}
					if len(replacement.recalls) != 0 {
						t.Fatal("confirmed recall repeated")
					}
					return
				}
				if text, bad := callTool(t, fresh.URL(), "token", tool, args); bad {
					t.Fatal(text)
				}
				if tool == "channel_recall" && len(replacement.recalls) != 1 {
					t.Fatal("new recall did not continue")
				}
				if tool == "channel_update" && len(replacement.updates) != 1 {
					t.Fatal("message update did not continue")
				}
			})
		}
	}
}

func TestCancelledReceiptReadDoesNotFenceGate(t *testing.T) {
	store := newGrantStore()
	scope := testScope()
	store.allowed[scope.AttemptID] = true
	s, _ := grantFixture(t, store)
	s.Extras("chat", "agent", "token", "")
	s.Anchor("chat", channel.Address{Channel: "feishu", Conversation: "chat", Message: "inbound"})
	if err := s.BindExecution(t.Context(), Binding{ConversationID: "chat", AgentID: "agent"}, scope); err != nil {
		t.Fatal(err)
	}
	fresh, _ := grantFixture(t, store)
	bind, authorized, err := fresh.authenticate(t.Context(), "token", true)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(authorized)
	cancel()
	if _, err := fresh.channelCall(ctx, bind, "channel_send", json.RawMessage(`{"content":"canceled"}`)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read: %v", err)
	}
	if status := rpc(t, fresh.URL(), "token", "initialize", nil).status; status != http.StatusOK {
		t.Fatalf("canceled read fenced healthy gate: %d", status)
	}
}
