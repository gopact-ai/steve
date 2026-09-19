package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/gopact-ai/steve/internal/channel"
	lark "github.com/larksuite/oapi-sdk-go/v3"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
)

func TestMessageCallbackPropagatesAcceptanceFailureThroughSDK(t *testing.T) {
	refused := errors.New("durable input rejected")
	c := &Channel{}
	event := messageEvent("user", "text", `{"text":"hello"}`)
	event.Event.Message.ChatType = ptr("p2p")
	// Use the SDK's actual event decoder and dispatch path, without a socket
	// or credentials. WS translates this returned error to code 500; its
	// server-side redelivery policy is not implemented by this SDK.
	body, err := json.Marshal(event.Event)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"schema":"2.0","header":{"event_type":"im.message.receive_v1"},"event":` + string(body) + `}`)
	called := 0
	d := dispatcher.NewEventDispatcher("", "").OnP2MessageReceiveV1(c.messageHandler("bot", false, func(msg InboundMessage) error {
		called++
		if msg.MessageID != "om_message" {
			t.Errorf("message identity changed: %+v", msg)
		}
		return refused
	}))
	if _, err := d.Do(context.Background(), payload); !errors.Is(err, refused) {
		t.Fatalf("callback confirmed rejected input: %v", err)
	}
	if called != 1 {
		t.Fatalf("callback returned before synchronous acceptance: calls=%d", called)
	}
}

func TestMessageCallbackDoesNotWaitForRemoteContextHydration(t *testing.T) {
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		http.Error(w, "context unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	c := &Channel{api: lark.NewClient(t.Name(), "test-only", lark.WithOpenBaseUrl(server.URL), lark.WithHttpClient(server.Client()))}
	event := messageEvent("user", "image", `{"image_key":"test-image"}`)
	event.Event.Message.ChatType = ptr("p2p")
	accepted := false
	err := c.messageHandler("bot", false, func(msg InboundMessage) error {
		accepted = true
		if len(msg.ImageKeys) != 1 {
			t.Fatalf("original image intent lost: %+v", msg)
		}
		return nil
	})(t.Context(), event)
	if err != nil || !accepted || reads.Load() != 0 {
		t.Fatalf("callback waited on external context instead of only accepting: reads=%d accepted=%t err=%v", reads.Load(), accepted, err)
	}
}

func TestReplyCardSuccessfulProviderResponseWithoutReceiptIsUnknown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
			_, _ = io.WriteString(w, `{"code":0,"tenant_access_token":"test-token","expire":7200}`)
			return
		}
		_, _ = io.WriteString(w, `{"code":0,"data":{}}`)
	}))
	defer server.Close()
	c := &Channel{api: lark.NewClient(t.Name(), "test-only", lark.WithOpenBaseUrl(server.URL), lark.WithHttpClient(server.Client()))}
	id, err := c.ReplyCard(t.Context(), "anchor", []byte(`{"schema":"2.0"}`))
	if id != "" || !errors.Is(err, channel.ErrOutcomeUnknown) {
		t.Fatalf("successful POST missing its receipt permits a blind fallback: %q %v", id, err)
	}
}

func TestMalformedProviderResponseCannotAuthorizeDeliveryFallback(t *testing.T) {
	for _, method := range []string{"card", "patch", "text", "topic"} {
		t.Run(method, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
					_, _ = io.WriteString(w, `{"code":0,"tenant_access_token":"test-token","expire":7200}`)
					return
				}
				// The provider received the effect, but its response cannot
				// prove either success or refusal. This is not a net.Error.
				_, _ = io.WriteString(w, `not-json`)
			}))
			defer server.Close()
			c := &Channel{api: lark.NewClient(t.Name(), "test-only", lark.WithOpenBaseUrl(server.URL), lark.WithHttpClient(server.Client()))}
			var err error
			switch method {
			case "card":
				_, err = c.ReplyCard(t.Context(), "anchor", []byte(`{"schema":"2.0"}`))
			case "patch":
				err = c.PatchCard(t.Context(), "anchor", []byte(`{"schema":"2.0"}`))
			case "text":
				_, err = c.ReplyText(t.Context(), "anchor", "result")
			case "topic":
				_, _, err = c.ReplyThread(t.Context(), "anchor", "task")
			}
			if !errors.Is(err, channel.ErrOutcomeUnknown) {
				t.Fatalf("undecodable response treated as safe refusal: %v", err)
			}
		})
	}
}
