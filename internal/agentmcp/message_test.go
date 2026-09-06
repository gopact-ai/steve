package agentmcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/intent"
	"github.com/gopact-ai/steve/internal/ledger"
)

type neutralMessenger struct {
	mu                      sync.Mutex
	sends, updates, recalls []channel.Address
	messages                []channel.Message
	err                     error
	empty                   bool
	before                  func()
}

func (m *neutralMessenger) Send(_ context.Context, to channel.Address, msg channel.Message) (string, error) {
	if m.before != nil {
		m.before()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sends = append(m.sends, to)
	m.messages = append(m.messages, msg)
	if m.err != nil {
		return "", m.err
	}
	if m.empty {
		return "", nil
	}
	return "same-provider-id", nil
}
func (m *neutralMessenger) Update(_ context.Context, to channel.Address, msg channel.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updates = append(m.updates, to)
	m.messages = append(m.messages, msg)
	return m.err
}
func (m *neutralMessenger) Recall(_ context.Context, to channel.Address) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recalls = append(m.recalls, to)
	return m.err
}

type taskInformer struct{ task string }

func (i *taskInformer) Context(context.Context, string, string) (ContextInfo, error) {
	return ContextInfo{Task: i.task, Mode: "owner"}, nil
}
func (*taskInformer) Projects(context.Context, string, string) (string, error) { return "", nil }

func attachEffects(t *testing.T, s *Server) (*intent.Service, *attemptsOf) {
	t.Helper()
	book, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	service := intent.New(book)
	attempts := &attemptsOf{live: "attempt-1"}
	s.SetIntents(intent.ForAgents{S: service, Attempts: attempts})
	return service, attempts
}

func TestMessageRoutingAndOpaqueReceipts(t *testing.T) {
	s, feishu := startServer(t)
	page, other := &neutralMessenger{}, &neutralMessenger{}
	s.BindChannel("console", page)
	s.BindChannel("other", other)
	s.Extras("console:side", "codex", "page-token", "")
	s.Anchor("console:side", channel.Address{Channel: "console", Conversation: "console:side", Message: "input"})
	out, bad := callTool(t, s.URL(), "page-token", "channel_send", map[string]any{"content": "literal **text**", "format": "text"})
	if bad {
		t.Fatal(out)
	}
	id := strings.TrimPrefix(out, "sent message_id=")
	if id == "same-provider-id" || !strings.HasPrefix(id, "msg_") {
		t.Fatalf("not an opaque receipt: %s", id)
	}
	if len(page.sends) != 1 || len(feishu.cards) != 0 || page.sends[0].Conversation != "console:side" || page.messages[0].Format != "text" {
		t.Fatal("default escaped the bound channel")
	}
	if _, bad = callTool(t, s.URL(), "page-token", "channel_send", map[string]any{"channel": "feishu", "content": "wrong"}); !bad {
		t.Fatal("explicit channel changed recipient")
	}
	s.SetDefaultChannel("other")
	for _, tool := range []string{"channel_update", "channel_recall"} {
		args := map[string]any{"message_id": id, "channel": "other"}
		if tool == "channel_update" {
			args["content"] = "changed"
		}
		if _, bad = callTool(t, s.URL(), "page-token", tool, args); !bad {
			t.Fatalf("%s redirected the receipt", tool)
		}
	}
	if out, bad = callTool(t, s.URL(), "page-token", "channel_update", map[string]any{"message_id": id, "content": "updated"}); bad {
		t.Fatal(out)
	}
	if page.updates[0].Message != "same-provider-id" || page.updates[0].Channel != "console" {
		t.Fatal("update lost provider receipt")
	}
	s.Extras("other-chat", "codex", "other-token", "")
	s.Anchor("other-chat", channel.Address{Conversation: "other-chat", Message: "other-input"})
	out, bad = callTool(t, s.URL(), "other-token", "channel_send", map[string]any{"content": "other"})
	if bad {
		t.Fatal(out)
	}
	otherID := strings.TrimPrefix(out, "sent message_id=")
	if otherID == id {
		t.Fatal("provider id collision leaked into handles")
	}
	if _, bad = callTool(t, s.URL(), "other-token", "channel_recall", map[string]any{"message_id": id}); !bad {
		t.Fatal("foreign handle authorized")
	}
	if out, bad = callTool(t, s.URL(), "page-token", "channel_recall", map[string]any{"message_id": id}); bad {
		t.Fatal(out)
	}
	if len(other.recalls) != 0 || page.recalls[0].Message != "same-provider-id" {
		t.Fatal("recall crossed channels")
	}
}

func TestMainSessionUnknownEffectUsesTaskAndCanonicalChannel(t *testing.T) {
	s, _ := startServer(t)
	messenger := &neutralMessenger{empty: true}
	s.BindChannel("console", messenger)
	service, attempts := attachEffects(t, s)
	s.SetInformer(&taskInformer{task: "main-task"})
	s.Extras("console:side", "codex", "main-token", "")
	s.Anchor("console:side", channel.Address{Channel: "console", Conversation: "console:side", Message: "input"})
	args := map[string]any{"content": "phase complete"}
	if _, bad := callTool(t, s.URL(), "main-token", "channel_send", args); !bad {
		t.Fatal("empty provider receipt reported success")
	}
	unknown, _ := service.Unresolved(context.Background())
	if len(unknown) != 1 || unknown[0].TaskID != "main-task" {
		t.Fatalf("main effect escaped task scope: %+v", unknown)
	}
	messenger.empty = false
	explicit := map[string]any{"content": "phase complete", "channel": "console", "format": "markdown", "mention": false, "progress": ""}
	for _, attempt := range []string{"attempt-1", "attempt-2"} {
		attempts.live = attempt
		out, bad := callTool(t, s.URL(), "main-token", "channel_send", explicit)
		if !bad || !strings.Contains(out, "blocked pending reconciliation") {
			t.Fatalf("unknown replay allowed: %s", out)
		}
	}
	if len(messenger.sends) != 1 {
		t.Fatalf("unknown send repeated %d times", len(messenger.sends))
	}
	if _, bad := callTool(t, s.URL(), "main-token", "feishu_send", args); !bad {
		t.Fatal("provider-specific alias remains callable")
	}
}

func TestRecallRejectsIgnoredArgumentsAndUnknownRetry(t *testing.T) {
	s, _ := startServer(t)
	messenger := &neutralMessenger{}
	s.BindChannel("console", messenger)
	service, _ := attachEffects(t, s)
	s.SetInformer(&taskInformer{task: "main-task"})
	s.Extras("console:side", "codex", "token", "")
	s.Anchor("console:side", channel.Address{Channel: "console", Conversation: "console:side", Message: "input"})
	out, bad := callTool(t, s.URL(), "token", "channel_send", map[string]any{"content": "phase"})
	if bad {
		t.Fatal(out)
	}
	id := strings.TrimPrefix(out, "sent message_id=")
	messenger.err = channel.ErrOutcomeUnknown
	if _, bad = callTool(t, s.URL(), "token", "channel_recall", map[string]any{"message_id": id}); !bad {
		t.Fatal("unknown recall reported success")
	}
	for _, extra := range []string{"content", "progress", "format"} {
		if _, bad = callTool(t, s.URL(), "token", "channel_recall", map[string]any{"message_id": id, extra: "ignored"}); !bad {
			t.Fatal("ignored args accepted")
		}
	}
	if out, bad = callTool(t, s.URL(), "token", "channel_recall", map[string]any{"message_id": id, "channel": "console"}); !bad || !strings.Contains(out, "blocked") {
		t.Fatalf("unknown recall bypass: %s", out)
	}
	if len(messenger.recalls) != 1 {
		t.Fatal("recall was dispatched again")
	}
	unknown, _ := service.Unresolved(context.Background())
	if len(unknown) != 1 {
		t.Fatal("unknown recall not recorded")
	}
}

func TestCanceledCallerDoesNotEraseConfirmedReceipt(t *testing.T) {
	s, _ := startServer(t)
	service, _ := attachEffects(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	messenger := &neutralMessenger{before: cancel}
	s.BindChannel("console", messenger)
	s.Anchor("console:side", channel.Address{Channel: "console", Conversation: "console:side", Message: "input"})
	out, err := s.channelCall(ctx, binding{conversationID: "console:side", agentID: "codex", taskID: "main-task"}, "channel_send", json.RawMessage(`{"content":"phase"}`))
	if err != nil {
		t.Fatal(err)
	}
	records, _ := service.ForTask(context.Background(), "main-task")
	if len(records) != 1 || records[0].State != intent.Succeeded || !strings.Contains(string(records[0].Receipt), "same-provider-id") || !strings.Contains(string(records[0].Receipt), out) {
		t.Fatalf("receipt lost: %+v", records)
	}
}

func TestLateSendKeepsOriginalTaskAndCannotMutateNextTurn(t *testing.T) {
	s, _ := startServer(t)
	messenger := &neutralMessenger{before: func() {
		s.Anchor("console:side", channel.Address{Channel: "console", Conversation: "console:side", Message: "next-input"})
	}}
	s.BindChannel("console", messenger)
	s.Anchor("console:side", channel.Address{Channel: "console", Conversation: "console:side", Message: "input"})
	var task string
	s.SetJournal(func(_, _, id string, _ channel.Address) { task = id })
	bind := binding{conversationID: "console:side", agentID: "codex", taskID: "original-task"}
	out, err := s.channelCall(context.Background(), bind, "channel_send", json.RawMessage(`{"content":"phase"}`))
	if err != nil {
		t.Fatal(err)
	}
	if task != "original-task" {
		t.Fatalf("late receipt moved to %s", task)
	}
	raw, _ := json.Marshal(messageArgs{MessageID: strings.TrimPrefix(out, "sent message_id=")})
	if _, err = s.channelCall(context.Background(), bind, "channel_recall", raw); err == nil {
		t.Fatal("late receipt became mutable in next turn")
	}
	if len(messenger.recalls) != 0 {
		t.Fatal("old receipt recalled after turn changed")
	}
}

func TestChannelCatalogIsTransportNeutral(t *testing.T) {
	s, _ := startServer(t)
	s.Extras("chat", "codex", "token", "")
	out := rpc(t, s.URL(), "token", "tools/list", nil)
	for _, name := range []string{"channel_send", "channel_update", "channel_recall"} {
		if !strings.Contains(out.rawBody, name) {
			t.Fatal("missing " + name)
		}
	}
	if strings.Contains(out.rawBody, "feishu_") {
		t.Fatal("provider tool leaked into catalogue")
	}
	if !errors.Is(errOutcomeUnknown, channel.ErrOutcomeUnknown) {
		t.Fatal("unknown errors are not channel-neutral")
	}
}
