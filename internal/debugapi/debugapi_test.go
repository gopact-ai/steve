package debugapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gopact-ai/steve/internal/channel/feishu"
	"github.com/gopact-ai/steve/internal/gateway"
	"github.com/gopact-ai/steve/internal/protocol"
)

type fakeGateway struct {
	msgs   chan feishu.InboundMessage
	action chan feishu.CardAction
}

func (f *fakeGateway) HandleMessage(msg feishu.InboundMessage) { f.msgs <- msg }

func (f *fakeGateway) HandleCardAction(action feishu.CardAction) feishu.CardToast {
	f.action <- action
	return feishu.CardToast{Type: "success", Content: "ok"}
}

func (f *fakeGateway) LiveTurns() []gateway.TurnInfo {
	return []gateway.TurnInfo{{ID: "t1", CardID: "om_card", Running: true, Text: "hi"}}
}

func (f *fakeGateway) LastCard() []byte { return []byte(`{"schema":"2.0"}`) }

type fakeSender struct{ text chan string }

func (s *fakeSender) SendChat(_ context.Context, chatID, text string) (feishu.Sent, error) {
	s.text <- text
	return feishu.Sent{ChatID: chatID, MessageID: "om_real"}, nil
}

func TestInjectMessageUsesDefaults(t *testing.T) {
	gw := &fakeGateway{msgs: make(chan feishu.InboundMessage, 1)}
	srv := httptest.NewServer(Handler(gw, Defaults{ChatID: "oc_chat", SenderOpenID: "ou_owner"}))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/message", "application/json", strings.NewReader(`{"text":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	msg := <-gw.msgs
	if msg.ChatID != "oc_chat" || msg.SenderOpenID != "ou_owner" || msg.Text != "hi" {
		t.Fatalf("message = %#v", msg)
	}
	if !msg.Mentioned || msg.ChatType != protocol.ChatGroup {
		t.Fatalf("defaults not applied: %#v", msg)
	}
	if !strings.HasPrefix(msg.MessageID, "om_debug_") {
		t.Fatalf("message id = %q", msg.MessageID)
	}
}

func TestInjectMessageSeedsRealMessage(t *testing.T) {
	gw := &fakeGateway{msgs: make(chan feishu.InboundMessage, 1)}
	sender := &fakeSender{text: make(chan string, 1)}
	srv := httptest.NewServer(Handler(gw, Defaults{ChatID: "oc_chat", Sender: sender}))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/message", "application/json", strings.NewReader(`{"text":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := <-sender.text; got != "hi" {
		t.Fatalf("seeded text = %q", got)
	}
	if msg := <-gw.msgs; msg.MessageID != "om_real" {
		t.Fatalf("turn did not use the real message id: %#v", msg)
	}
}

func TestInjectCardAction(t *testing.T) {
	gw := &fakeGateway{action: make(chan feishu.CardAction, 1)}
	srv := httptest.NewServer(Handler(gw, Defaults{SenderOpenID: "ou_owner"}))
	defer srv.Close()

	body := `{"action":"tool_approval","request_id":"req_1","decision":"allow"}`
	resp, err := http.Post(srv.URL+"/card", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	action := <-gw.action
	if action.Action != "tool_approval" || action.RequestID != "req_1" || action.OpenID != "ou_owner" {
		t.Fatalf("action = %#v", action)
	}
}

func TestServeRejectsNonLoopback(t *testing.T) {
	if err := checkLoopback("0.0.0.0:7788"); err == nil {
		t.Fatal("expected non-loopback address to be rejected")
	}
	if err := checkLoopback("127.0.0.1:7788"); err != nil {
		t.Fatalf("loopback rejected: %v", err)
	}
}
