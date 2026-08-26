package feishu

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// waitDeadline bounds how long a test waits for a goroutine to reach a known
// point. A passing test never spends it; a generous bound is what keeps the
// suite usable under -race, where everything runs several times slower.
const waitDeadline = 10 * time.Second

type stuckConn struct {
	closed  chan struct{}
	closeN  atomic.Bool
	startN  sync.Once
	started chan struct{}
}

func newStuckConn() *stuckConn {
	return &stuckConn{closed: make(chan struct{}), started: make(chan struct{})}
}

func (s *stuckConn) Start(context.Context) error {
	s.startN.Do(func() { close(s.started) })
	<-s.closed
	return nil
}

func (s *stuckConn) Close() {
	if s.closeN.CompareAndSwap(false, true) {
		close(s.closed)
	}
}

type failConn struct{}

func (failConn) Start(context.Context) error { return errors.New("boom") }
func (failConn) Close()                      {}

func TestSendRequiresReceiveID(t *testing.T) {
	c := &Channel{}
	if _, err := c.Send(t.Context(), "", "hi"); err == nil {
		t.Fatal("expected receive id error")
	}
}

func TestNormalizeTextMessage(t *testing.T) {
	event := messageEvent("user", "text", `{"text":"@_user_1 hello"}`)
	event.Event.Message.Mentions = []*larkim.MentionEvent{{MentionedType: ptr("bot"), Id: &larkim.UserId{OpenId: ptr("ou_bot")}}}

	msg, ok := normalize(event, "ou_bot", false)
	if !ok {
		t.Fatal("normalize rejected a text message")
	}
	if msg.ChatID != "oc_chat" || msg.MessageID != "om_message" || msg.Text != "hello" {
		t.Fatalf("unexpected message: %#v", msg)
	}
	if !msg.Mentioned {
		t.Fatal("bot mention was not recorded")
	}
}

func TestNormalizeIgnoresUnmentionedGroupMessage(t *testing.T) {
	if _, ok := normalize(messageEvent("user", "text", `{"text":"hello"}`), "ou_bot", false); ok {
		t.Fatal("normalize accepted an unmentioned group message")
	}
}

func TestNormalizeAllowsUnmentionedGroupWhenConfigured(t *testing.T) {
	msg, ok := normalize(messageEvent("user", "text", `{"text":"hello"}`), "ou_bot", true)
	if !ok || msg.Text != "hello" {
		t.Fatalf("expected unmentioned group message, got %#v %v", msg, ok)
	}
	if msg.Mentioned {
		t.Fatal("unmentioned group message marked mentioned")
	}
}

func TestNormalizeStripsAllMention(t *testing.T) {
	event := messageEvent("user", "text", `{"text":"@_all hello"}`)
	event.Event.Message.Mentions = []*larkim.MentionEvent{{MentionedType: ptr("bot"), Id: &larkim.UserId{OpenId: ptr("ou_bot")}}}
	msg, ok := normalize(event, "ou_bot", false)
	if !ok || msg.Text != "hello" {
		t.Fatalf("unexpected message: %#v, %v", msg, ok)
	}
}

func TestNormalizeIgnoresMentionOfAnotherBot(t *testing.T) {
	event := messageEvent("user", "text", `{"text":"@_user_1 hello"}`)
	event.Event.Message.Mentions = []*larkim.MentionEvent{{MentionedType: ptr("bot"), Id: &larkim.UserId{OpenId: ptr("ou_other")}}}
	if _, ok := normalize(event, "ou_bot", false); ok {
		t.Fatal("normalize accepted a mention of another bot")
	}
}

func TestNormalizeUsesThreadAsConversation(t *testing.T) {
	event := messageEvent("user", "text", `{"text":"@_user_1 hello"}`)
	event.Event.Message.Mentions = []*larkim.MentionEvent{{MentionedType: ptr("bot"), Id: &larkim.UserId{OpenId: ptr("ou_bot")}}}
	event.Event.Message.ThreadId = ptr("omt_thread")
	msg, ok := normalize(event, "ou_bot", false)
	if !ok || msg.ConversationID != "omt_thread" {
		t.Fatalf("unexpected message: %#v, %v", msg, ok)
	}
}

func TestNormalizeIgnoresBotMessages(t *testing.T) {
	if _, ok := normalize(messageEvent("bot", "text", `{"text":"loop"}`), "ou_bot", false); ok {
		t.Fatal("normalize accepted a bot message")
	}
}

func TestNormalizeImageMessage(t *testing.T) {
	msg, ok := normalize(messageEvent("user", "image", `{"image_key":"img_1"}`), "ou_bot", true)
	if !ok || msg.Text != "" || len(msg.ImageKeys) != 1 || msg.ImageKeys[0] != "img_1" {
		t.Fatalf("unexpected image message: %#v %v", msg, ok)
	}
}

func TestNormalizePostExtractsTextAndImages(t *testing.T) {
	content := `{"title":"看这个","content":[[{"tag":"text","text":"@_user_1 修卡片"},{"tag":"img","image_key":"img_2"}]]}`
	event := messageEvent("user", "post", content)
	event.Event.Message.Mentions = []*larkim.MentionEvent{{MentionedType: ptr("bot"), Id: &larkim.UserId{OpenId: ptr("ou_bot")}}}
	msg, ok := normalize(event, "ou_bot", false)
	if !ok || msg.Text != "看这个 修卡片" || len(msg.ImageKeys) != 1 || msg.ImageKeys[0] != "img_2" {
		t.Fatalf("unexpected post message: %#v %v", msg, ok)
	}
}

func TestNormalizeDropsEmptyText(t *testing.T) {
	if _, ok := normalize(messageEvent("user", "text", `{"text":"   "}`), "ou_bot", true); ok {
		t.Fatal("normalize accepted empty text")
	}
}

func TestNormalizeKeepsParentID(t *testing.T) {
	event := messageEvent("user", "text", `{"text":"再试下这个"}`)
	event.Event.Message.ParentId = ptr("om_parent")
	msg, ok := normalize(event, "ou_bot", true)
	if !ok || msg.ParentID != "om_parent" {
		t.Fatalf("unexpected message: %#v %v", msg, ok)
	}
}

func TestParseContentSkipsUnsupportedType(t *testing.T) {
	if _, _, ok := parseContent("interactive", `{}`); ok {
		t.Fatal("interactive should not produce content")
	}
}

func TestParseCardAction(t *testing.T) {
	event := &callback.CardActionTriggerEvent{Event: &callback.CardActionTriggerRequest{
		Operator: &callback.Operator{OpenID: "ou_sender"},
		Context:  &callback.Context{OpenMessageID: "om_card", OpenChatID: "oc_chat"},
		Action: &callback.CallBackAction{Value: map[string]any{
			"action": "tool_approval", "request_id": "req_1", "decision": "allow",
		}},
	}}
	got := parseCardAction(event)
	if got.OpenID != "ou_sender" || got.MessageID != "om_card" || got.RequestID != "req_1" || got.Decision != "allow" {
		t.Fatalf("unexpected action: %#v", got)
	}
}

func TestReactionRequiresIDs(t *testing.T) {
	c := &Channel{}
	if _, err := c.AddReaction(t.Context(), "", "THINKING"); err == nil {
		t.Fatal("expected empty message id to fail")
	}
	if _, err := c.AddReaction(t.Context(), "om_message", ""); err == nil {
		t.Fatal("expected empty emoji to fail")
	}
	if err := c.RemoveReaction(t.Context(), "om_message", ""); err == nil {
		t.Fatal("expected empty reaction id to fail")
	}
}

func TestCardRequiresIDs(t *testing.T) {
	c := &Channel{}
	if _, err := c.ReplyCard(t.Context(), "", []byte(`{}`)); err == nil {
		t.Fatal("expected empty message id to fail")
	}
	if _, err := c.ReplyCard(t.Context(), "om_message", nil); err == nil {
		t.Fatal("expected empty payload to fail")
	}
	if err := c.PatchCard(t.Context(), "", []byte(`{}`)); err == nil {
		t.Fatal("expected empty message id to fail")
	}
}

func TestStartReturnsWhenContextCanceled(t *testing.T) {
	conn := newStuckConn()
	c := &Channel{ws: conn}
	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() { errCh <- c.Start(ctx) }()

	select {
	case <-conn.started:
	case <-time.After(waitDeadline):
		t.Fatal("Start did not begin")
	}
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Start() = %v, want context.Canceled", err)
		}
	case <-time.After(waitDeadline):
		t.Fatal("Start did not return after cancel")
	}
	if !conn.closeN.Load() {
		t.Fatal("cancel did not close the long connection")
	}
}

func TestStartReturnsConnectError(t *testing.T) {
	err := (&Channel{ws: failConn{}}).Start(t.Context())
	if err == nil || err.Error() != "boom" {
		t.Fatalf("Start() = %v, want boom", err)
	}
}

func messageEvent(senderType, messageType, content string) *larkim.P2MessageReceiveV1 {
	return &larkim.P2MessageReceiveV1{Event: &larkim.P2MessageReceiveV1Data{
		Sender: &larkim.EventSender{
			SenderType: &senderType,
			SenderId:   &larkim.UserId{OpenId: ptr("ou_sender")},
		},
		Message: &larkim.EventMessage{
			ChatId:      ptr("oc_chat"),
			ChatType:    ptr("group"),
			MessageId:   ptr("om_message"),
			MessageType: &messageType,
			Content:     &content,
		},
	}}
}

func ptr[T any](value T) *T { return &value }
