package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/gopact-ai/steve/internal/channel"
	lark "github.com/larksuite/oapi-sdk-go/v3"
)

type messageCall struct {
	kind, target, text string
	payload            []byte
}

type messageAPIRecorder struct {
	calls []messageCall
	err   error
}

func (r *messageAPIRecorder) ReplyCard(_ context.Context, target string, payload []byte) (string, error) {
	r.calls = append(r.calls, messageCall{kind: "card", target: target, payload: payload})
	return "om_sent", r.err
}

func (r *messageAPIRecorder) ReplyText(_ context.Context, target, text string) (string, error) {
	r.calls = append(r.calls, messageCall{kind: "text", target: target, text: text})
	return "om_sent", r.err
}

func (r *messageAPIRecorder) PatchCard(_ context.Context, target string, payload []byte) error {
	r.calls = append(r.calls, messageCall{kind: "update", target: target, payload: payload})
	return r.err
}

func (r *messageAPIRecorder) DeleteMessage(_ context.Context, target string) error {
	r.calls = append(r.calls, messageCall{kind: "recall", target: target})
	return r.err
}

var messageAddress = channel.Address{Channel: "feishu", Conversation: "thread", Message: "om_anchor"}

func TestMessengerRendersMarkdownOnlyAtFeishuBoundary(t *testing.T) {
	api := &messageAPIRecorder{}
	m := Messenger{API: api}
	msg := channel.Message{Content: "## 阶段完成\n已验证 <at id=\"all\">所有人</at>", Attribution: "builder · <AT id='ou_owner'>owner</AT> · 里程碑 1/2"}
	id, err := m.Send(t.Context(), messageAddress, msg)
	if err != nil || id != "om_sent" || len(api.calls) != 1 {
		t.Fatalf("send id=%q err=%v calls=%+v", id, err, api.calls)
	}
	call := api.calls[0]
	if call.kind != "card" || call.target != "om_anchor" {
		t.Fatalf("wrong provider operation: %+v", call)
	}
	var card struct {
		Schema string `json:"schema"`
		Config struct {
			UpdateMulti bool                     `json:"update_multi"`
			Summary     struct{ Content string } `json:"summary"`
		} `json:"config"`
		Body struct {
			Elements []struct{ Tag, Content string } `json:"elements"`
		} `json:"body"`
	}
	if err := json.Unmarshal(call.payload, &card); err != nil {
		t.Fatal(err)
	}
	if card.Schema != "2.0" || !card.Config.UpdateMulti || len(card.Body.Elements) != 2 {
		t.Fatalf("invalid milestone card: %s", call.payload)
	}
	if card.Body.Elements[0].Tag != "markdown" || card.Body.Elements[0].Content != "## 阶段完成\n已验证 所有人" || card.Config.Summary.Content != "## 阶段完成 已验证 所有人" {
		t.Fatalf("wrong card body/summary: %s", call.payload)
	}
	if card.Body.Elements[1].Content != "<font color='grey'>builder · owner · 里程碑 1/2</font>" {
		t.Fatalf("wrong attribution: %s", call.payload)
	}
	if !strings.Contains(msg.Content, "<at") {
		t.Fatal("adapter changed the caller's neutral message")
	}
}

func TestMessengerTextStaysTextAndRemovesMentions(t *testing.T) {
	api := &messageAPIRecorder{}
	m := Messenger{API: api}
	id, err := m.Send(t.Context(), messageAddress, channel.Message{Format: "text", Content: "hi <at user_id='ou_owner'>用户</at>", Attribution: "builder"})
	if err != nil || id != "om_sent" || len(api.calls) != 1 || api.calls[0].kind != "text" || api.calls[0].text != "hi 用户" {
		t.Fatalf("text send id=%q err=%v calls=%+v", id, err, api.calls)
	}
}

func TestMessengerEditsAndRecallsTheProviderReceipt(t *testing.T) {
	api := &messageAPIRecorder{}
	m := Messenger{API: api}
	address := messageAddress
	address.Message = "om_sent"
	if err := m.Update(t.Context(), address, channel.Message{Format: "markdown", Content: "完成 <at id=all>所有人</at>", Attribution: "builder · 里程碑 2/2"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Recall(t.Context(), address); err != nil {
		t.Fatal(err)
	}
	if len(api.calls) != 2 || api.calls[0].kind != "update" || api.calls[1].kind != "recall" || api.calls[0].target != "om_sent" || api.calls[1].target != "om_sent" {
		t.Fatalf("wrong operations: %+v", api.calls)
	}
	var payload map[string]any
	if err := json.Unmarshal(api.calls[0].payload, &payload); err != nil {
		t.Fatal(err)
	}
	encoded := string(api.calls[0].payload)
	if strings.Contains(encoded, "id=all") || !strings.Contains(encoded, "2/2") {
		t.Fatalf("update lost mention protection/attribution: %s", encoded)
	}
}

func TestMessengerRejectsInvalidRoutesAndUnsupportedUpdatesBeforeIO(t *testing.T) {
	for _, address := range []channel.Address{
		{Channel: "console", Conversation: "thread", Message: "om_anchor"},
		{Channel: "feishu", Message: "om_anchor"},
		{Channel: "feishu", Conversation: "thread"},
	} {
		api := &messageAPIRecorder{}
		m := Messenger{API: api}
		if _, err := m.Send(t.Context(), address, channel.Message{Content: "news"}); err == nil {
			t.Fatalf("send accepted %+v", address)
		}
		if err := m.Update(t.Context(), address, channel.Message{Content: "news"}); err == nil {
			t.Fatalf("update accepted %+v", address)
		}
		if err := m.Recall(t.Context(), address); err == nil {
			t.Fatalf("recall accepted %+v", address)
		}
		if len(api.calls) != 0 {
			t.Fatalf("invalid route reached provider: %+v", api.calls)
		}
	}
	api := &messageAPIRecorder{}
	m := Messenger{API: api}
	if err := m.Update(t.Context(), messageAddress, channel.Message{Format: "text", Content: "plain"}); err == nil || !strings.Contains(err.Error(), "markdown") {
		t.Fatalf("text update must fail explicitly: %v", err)
	}
	if _, err := m.Send(t.Context(), messageAddress, channel.Message{Format: "html", Content: "news"}); err == nil {
		t.Fatal("accepted unsupported format")
	}
	if _, err := m.Send(t.Context(), messageAddress, channel.Message{Content: "<at id=all></at>"}); err == nil {
		t.Fatal("accepted empty content after mention cleanup")
	}
	if len(api.calls) != 0 {
		t.Fatalf("invalid content reached provider: %+v", api.calls)
	}
}

func TestMessengerMarksTransportFailuresAsUnknown(t *testing.T) {
	for name, cause := range map[string]error{
		"deadline":      context.DeadlineExceeded,
		"cancelled":     context.Canceled,
		"eof":           io.EOF,
		"unexpectedEOF": io.ErrUnexpectedEOF,
		"connectionReset": &net.OpError{
			Op: "read", Net: "tcp", Err: syscall.ECONNRESET,
		},
	} {
		t.Run(name, func(t *testing.T) {
			api := &messageAPIRecorder{err: fmt.Errorf("provider request: %w", cause)}
			m := Messenger{API: api}
			_, markdownErr := m.Send(t.Context(), messageAddress, channel.Message{Content: "news"})
			_, textErr := m.Send(t.Context(), messageAddress, channel.Message{Content: "news", Format: "text"})
			updateErr := m.Update(t.Context(), messageAddress, channel.Message{Content: "news"})
			recallErr := m.Recall(t.Context(), messageAddress)
			for operation, err := range map[string]error{"markdown": markdownErr, "text": textErr, "update": updateErr, "recall": recallErr} {
				if !errors.Is(err, channel.ErrOutcomeUnknown) || !errors.Is(err, cause) {
					t.Errorf("%s must preserve unknown status and original cause: %v", operation, err)
				}
			}
		})
	}
}

func TestMessengerKeepsDefiniteRefusalsDistinct(t *testing.T) {
	cause := errors.New("provider refused: message cannot be edited")
	api := &messageAPIRecorder{err: cause}
	m := Messenger{API: api}
	_, sendErr := m.Send(t.Context(), messageAddress, channel.Message{Content: "news"})
	for operation, err := range map[string]error{
		"send":   sendErr,
		"update": m.Update(t.Context(), messageAddress, channel.Message{Content: "news"}),
		"recall": m.Recall(t.Context(), messageAddress),
	} {
		if !errors.Is(err, cause) || errors.Is(err, channel.ErrOutcomeUnknown) {
			t.Errorf("%s must preserve a definite provider refusal: %v", operation, err)
		}
	}
	api.calls = nil
	wrongChannel := messageAddress
	wrongChannel.Channel = "console"
	_, routeErr := m.Send(t.Context(), wrongChannel, channel.Message{Content: "news"})
	_, formatErr := m.Send(t.Context(), messageAddress, channel.Message{Content: "news", Format: "html"})
	_, contentErr := m.Send(t.Context(), messageAddress, channel.Message{Content: "<at id=all></at>"})
	for kind, err := range map[string]error{"address": routeErr, "format": formatErr, "content": contentErr} {
		if err == nil || errors.Is(err, channel.ErrOutcomeUnknown) {
			t.Errorf("%s validation must be a definite refusal: %v", kind, err)
		}
	}
	if len(api.calls) != 0 {
		t.Fatalf("preflight refusal reached the provider: %+v", api.calls)
	}
}

func TestPatchCardDoesNotRetryAnUncertainRequest(t *testing.T) {
	var patches, updates atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
			_, _ = io.WriteString(w, `{"code":0,"tenant_access_token":"test-token","expire":7200}`)
			return
		}
		if r.URL.Path != "/open-apis/im/v1/messages/om_anchor" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch r.Method {
		case http.MethodPatch:
			patches.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			// The provider received the complete request but its response was lost.
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = conn.Close()
		case http.MethodPut:
			updates.Add(1)
			_, _ = io.WriteString(w, `{"code":230001,"msg":"update refused"}`)
		default:
			t.Errorf("unexpected method: %s", r.Method)
		}
	}))
	t.Cleanup(server.Close)
	c := &Channel{api: lark.NewClient(t.Name(), "test-secret", lark.WithOpenBaseUrl(server.URL), lark.WithHttpClient(server.Client()))}
	defer func() {
		if value := recover(); value != nil {
			t.Fatalf("uncertain PATCH with no response must not panic: %v", value)
		}
	}()
	err := (Messenger{API: c}).Update(t.Context(), messageAddress, channel.Message{Content: "news"})
	if !errors.Is(err, channel.ErrOutcomeUnknown) || !errors.Is(err, io.EOF) {
		t.Fatalf("must preserve the uncertain transport error: %v", err)
	}
	if patches.Load() != 1 || updates.Load() != 0 {
		t.Fatalf("uncertain request was retried: PATCH=%d PUT=%d", patches.Load(), updates.Load())
	}
}

func TestPatchCardFallsBackAfterDefiniteProviderRefusal(t *testing.T) {
	for _, updateFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("updateFails=%t", updateFails), func(t *testing.T) {
			var patches, updates atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
					_, _ = io.WriteString(w, `{"code":0,"tenant_access_token":"test-token","expire":7200}`)
					return
				}
				switch r.Method {
				case http.MethodPatch:
					patches.Add(1)
					_, _ = io.WriteString(w, `{"code":230001,"msg":"patch refused"}`)
				case http.MethodPut:
					updates.Add(1)
					if updateFails {
						_, _ = io.WriteString(w, `{"code":230002,"msg":"update refused"}`)
					} else {
						_, _ = io.WriteString(w, `{"code":0}`)
					}
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
			}))
			t.Cleanup(server.Close)
			c := &Channel{api: lark.NewClient(t.Name(), "test-secret", lark.WithOpenBaseUrl(server.URL), lark.WithHttpClient(server.Client()))}
			err := (Messenger{API: c}).Update(t.Context(), messageAddress, channel.Message{Content: "news"})
			if (err != nil) != updateFails || errors.Is(err, channel.ErrOutcomeUnknown) {
				t.Fatalf("definite provider response misclassified: %v", err)
			}
			if updateFails && !strings.Contains(err.Error(), "230002") {
				t.Fatalf("lost fallback refusal: %v", err)
			}
			if patches.Load() != 1 || updates.Load() != 1 {
				t.Fatalf("definite refusal fallback changed: PATCH=%d PUT=%d", patches.Load(), updates.Load())
			}
		})
	}
}
