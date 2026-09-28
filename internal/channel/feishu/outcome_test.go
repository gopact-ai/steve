package feishu

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gopact-ai/steve/internal/channel"
)

// outboundCalls are the Channel calls that change what a Feishu chat shows.
var outboundCalls = map[string]func(context.Context, *Channel) error{
	"card": func(ctx context.Context, c *Channel) error {
		_, err := c.ReplyCard(ctx, "om_anchor", []byte(`{"schema":"2.0"}`))
		return err
	},
	"patch": func(ctx context.Context, c *Channel) error {
		return c.PatchCard(ctx, "om_anchor", []byte(`{"schema":"2.0"}`))
	},
	"text": func(ctx context.Context, c *Channel) error {
		_, err := c.ReplyText(ctx, "om_anchor", "result")
		return err
	},
	"topic": func(ctx context.Context, c *Channel) error {
		_, _, err := c.ReplyThread(ctx, "om_anchor", "task")
		return err
	},
	"delete": func(ctx context.Context, c *Channel) error {
		return c.DeleteMessage(ctx, "om_anchor")
	},
	"send": func(ctx context.Context, c *Channel) error {
		_, err := c.SendChat(ctx, "oc_chat", "hello")
		return err
	},
}

// channelAt is a Channel whose API calls go to url through client.
func channelAt(t *testing.T, url string, client *http.Client) *Channel {
	t.Helper()
	return &Channel{api: apiAt(t.Name(), "test-secret", url, client)}
}

// dropConnection closes the request's connection without answering.
func dropConnection(t *testing.T, w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		t.Errorf("hijack: %v", err)
		return
	}
	_ = conn.Close()
}

// A call whose request never reached Feishu is a definite failure, also to
// callers that read transport errors as an uncertain outcome: nothing was
// posted, so the caller may post again.
func TestOutboundCallsThatNeverReachFeishuAreDefinite(t *testing.T) {
	causes := map[string]func(t *testing.T) (url string, client *http.Client, reached *atomic.Int32){
		"token refused": func(t *testing.T) (string, *http.Client, *atomic.Int32) {
			var reached atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == tokenPath {
					jsonReply(w, http.StatusOK, `{"code":10014,"msg":"app secret invalid"}`)
					return
				}
				reached.Add(1)
				jsonReply(w, http.StatusOK, `{"code":0,"data":{"message_id":"om_sent","chat_id":"oc_chat"}}`)
			}))
			t.Cleanup(server.Close)
			return server.URL, server.Client(), &reached
		},
		"token lost": func(t *testing.T) (string, *http.Client, *atomic.Int32) {
			var reached atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == tokenPath {
					dropConnection(t, w, r)
					return
				}
				reached.Add(1)
				jsonReply(w, http.StatusOK, `{"code":0,"data":{"message_id":"om_sent","chat_id":"oc_chat"}}`)
			}))
			t.Cleanup(server.Close)
			return server.URL, server.Client(), &reached
		},
		"unreachable": func(t *testing.T) (string, *http.Client, *atomic.Int32) {
			server := httptest.NewServer(http.NotFoundHandler())
			server.Close()
			return server.URL, server.Client(), new(atomic.Int32)
		},
	}
	for cause, serve := range causes {
		for name, call := range outboundCalls {
			t.Run(cause+"/"+name, func(t *testing.T) {
				url, client, reached := serve(t)
				err := call(t.Context(), channelAt(t, url, client))
				if err == nil {
					t.Fatal("a call that never reached Feishu succeeded")
				}
				if reached.Load() != 0 {
					t.Fatalf("the API request reached Feishu %d times", reached.Load())
				}
				if errors.Is(err, channel.ErrOutcomeUnknown) || errors.Is(messageOutcome(err), channel.ErrOutcomeUnknown) {
					t.Fatalf("a request that never reached Feishu is reported as possibly delivered: %v", err)
				}
			})
		}
	}
}

// expiring is a context whose deadline passes when expire is closed, so a
// test decides exactly when a call times out.
type expiring struct {
	context.Context
	expire chan struct{}
}

func (e expiring) Done() <-chan struct{} { return e.expire }

func (e expiring) Err() error {
	select {
	case <-e.expire:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

// A call whose request may have reached Feishu cannot be reported as a
// definite failure: posting again could post twice.
func TestOutboundCallsThatMayHaveReachedFeishuAreUnknown(t *testing.T) {
	causes := map[string]func(w http.ResponseWriter, r *http.Request, expire func()){
		"response lost": func(w http.ResponseWriter, r *http.Request, _ func()) {
			dropConnection(t, w, r)
		},
		"server error": func(w http.ResponseWriter, _ *http.Request, _ func()) {
			jsonReply(w, http.StatusInternalServerError, `{"code":1000,"msg":"internal error"}`)
		},
		"timeout": func(_ http.ResponseWriter, r *http.Request, expire func()) {
			// Reading the whole request lets the server see the caller leave.
			_, _ = io.Copy(io.Discard, r.Body)
			expire()
			<-r.Context().Done()
		},
	}
	for cause, answer := range causes {
		for name, call := range outboundCalls {
			t.Run(cause+"/"+name, func(t *testing.T) {
				ctx := expiring{Context: t.Context(), expire: make(chan struct{})}
				var once sync.Once
				expire := func() { once.Do(func() { close(ctx.expire) }) }
				var reached atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == tokenPath {
						jsonReply(w, http.StatusOK, tokenOK)
						return
					}
					if reached.Add(1) > 1 {
						t.Errorf("an uncertain request was sent again: %s %s", r.Method, r.URL.Path)
					}
					answer(w, r, expire)
				}))
				t.Cleanup(server.Close)
				err := call(ctx, channelAt(t, server.URL, server.Client()))
				if !errors.Is(err, channel.ErrOutcomeUnknown) {
					t.Fatalf("a request that may have reached Feishu is reported as a definite failure: %v", err)
				}
			})
		}
	}
}

// A refusal Feishu answered is a definite failure and keeps its code.
func TestOutboundCallsKeepDefiniteRefusals(t *testing.T) {
	for name, call := range outboundCalls {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == tokenPath {
					jsonReply(w, http.StatusOK, tokenOK)
					return
				}
				jsonReply(w, http.StatusBadRequest, `{"code":230002,"msg":"refused"}`)
			}))
			t.Cleanup(server.Close)
			err := call(t.Context(), channelAt(t, server.URL, server.Client()))
			if err == nil || errors.Is(err, channel.ErrOutcomeUnknown) || !strings.Contains(err.Error(), "230002") {
				t.Fatalf("definite refusal misreported: %v", err)
			}
		})
	}
}
