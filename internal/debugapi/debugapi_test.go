package debugapi

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/channel/feishu"
	"github.com/gopact-ai/steve/internal/gateway"
	"github.com/gopact-ai/steve/internal/protocol"
)

type fakeGateway struct {
	msgs   chan feishu.InboundMessage
	action chan feishu.CardAction
	err    error
}

func (f *fakeGateway) HandleMessage(msg feishu.InboundMessage) error {
	if f.err != nil {
		return f.err
	}
	f.msgs <- msg
	return nil
}

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

// The endpoint serves only from a loopback IP: an address that names
// loopback but binds beyond it is refused, and what it bound is closed.
// listen stands in for the resolver, so a name binds where the test says on
// every platform; the context is already done, so a served endpoint returns.
func TestServeRefusesABindingBeyondLoopback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for bind, refused := range map[string]bool{"0.0.0.0:0": true, "127.0.0.1:0": false} {
		var bound *net.TCPListener
		listen := func(network, _ string) (net.Listener, error) {
			listener, err := net.Listen(network, bind)
			if err == nil {
				bound = listener.(*net.TCPListener)
			}
			return listener, err
		}
		err := serve(ctx, "localhost:7788", &fakeGateway{}, Defaults{}, listen)
		if refused != (err != nil) || refused && !strings.Contains(err.Error(), "localhost:7788") {
			t.Errorf("localhost:7788 bound to %s: err = %v, want refused: %v", bind, err, refused)
		}
		if bound == nil {
			t.Fatalf("localhost:7788: nothing was bound to %s", bind)
		}
		_ = bound.SetDeadline(time.Now())
		if _, err := bound.Accept(); !errors.Is(err, net.ErrClosed) {
			_ = bound.Close()
			t.Errorf("localhost:7788 bound to %s: the listener was left open (%v)", bind, err)
		}
	}
}

func TestInjectMessageDoesNotAcknowledgeRejectedAcceptance(t *testing.T) {
	gw := &fakeGateway{err: errors.New("durable acceptance refused")}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/message", strings.NewReader(`{"text":"hi","message_id":"original"}`))
	req.Host = "127.0.0.1:7711"
	Handler(gw, Defaults{ChatID: "chat", SenderOpenID: "owner"}).ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "original") {
		t.Fatalf("rejected input was accepted or lost retry identity: %d %s", rec.Code, rec.Body.String())
	}
}

// Loopback keeps other machines out, not pages open in the owner's browser.
func TestInjectionRefusesOtherSites(t *testing.T) {
	gw := &fakeGateway{msgs: make(chan feishu.InboundMessage, 1), action: make(chan feishu.CardAction, 1)}
	srv := httptest.NewServer(Handler(gw, Defaults{ChatID: "oc_chat", SenderOpenID: "ou_owner"}))
	defer srv.Close()
	for _, tc := range []struct{ path, host, origin string }{
		{"/message", "", "https://evil.example"},
		{"/card", "", "https://evil.example"},
		{"/message", "evil.example", ""},
	} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+tc.path, strings.NewReader(`{"text":"hi"}`))
		req.Header.Set("Content-Type", "text/plain")
		if tc.host != "" {
			req.Host = tc.host
		}
		if tc.origin != "" {
			req.Header.Set("Origin", tc.origin)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%+v: status = %d, want 403", tc, resp.StatusCode)
		}
	}
	if len(gw.msgs) != 0 || len(gw.action) != 0 {
		t.Fatal("a refused request reached the gateway")
	}
}
