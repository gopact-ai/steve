package feishu

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"

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

// A call Feishu never took is a definite failure, also to callers that read
// transport errors as an uncertain outcome: its request never reached
// Feishu, or Feishu turned it away for its token, so nothing was posted and
// the caller may post again.
func TestOutboundCallsFeishuNeverTookAreDefinite(t *testing.T) {
	causes := map[string]func(t *testing.T) (c *Channel, posted *atomic.Int32){
		"token refused": func(t *testing.T) (*Channel, *atomic.Int32) {
			var posted atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == tokenPath {
					jsonReply(w, http.StatusOK, `{"code":10014,"msg":"app secret invalid"}`)
					return
				}
				posted.Add(1)
				jsonReply(w, http.StatusOK, `{"code":0,"data":{"message_id":"om_sent","chat_id":"oc_chat"}}`)
			}))
			t.Cleanup(server.Close)
			return channelAt(t, server.URL, server.Client()), &posted
		},
		"token lost": func(t *testing.T) (*Channel, *atomic.Int32) {
			var posted atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == tokenPath {
					dropConnection(t, w, r)
					return
				}
				posted.Add(1)
				jsonReply(w, http.StatusOK, `{"code":0,"data":{"message_id":"om_sent","chat_id":"oc_chat"}}`)
			}))
			t.Cleanup(server.Close)
			return channelAt(t, server.URL, server.Client()), &posted
		},
		"unreachable": func(t *testing.T) (*Channel, *atomic.Int32) {
			server := httptest.NewServer(http.NotFoundHandler())
			server.Close()
			return channelAt(t, server.URL, server.Client()), new(atomic.Int32)
		},
		// With a token cached, the request that cannot be dialed is the API
		// request itself.
		"unreachable with a cached token": func(t *testing.T) (*Channel, *atomic.Int32) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == tokenPath {
					jsonReply(w, http.StatusOK, tokenOK)
					return
				}
				jsonReply(w, http.StatusOK, `{"code":0,"data":{"message_id":"om_sent","chat_id":"oc_chat"}}`)
			}))
			c := channelAt(t, server.URL, server.Client())
			if _, err := c.SendChat(t.Context(), "oc_chat", "hello"); err != nil {
				t.Fatalf("cache a token: %v", err)
			}
			server.Close()
			return c, new(atomic.Int32)
		},
		// Feishu rejects the token the API request carried, and the token
		// fetched to retry it is refused.
		"token rejected, then refused": func(t *testing.T) (*Channel, *atomic.Int32) {
			var tokens, rejected, posted atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == tokenPath {
					if tokens.Add(1) == 1 {
						// Expiring at once, the token is fetched again for the retry.
						jsonReply(w, http.StatusOK, `{"code":0,"tenant_access_token":"test-token","expire":0}`)
						return
					}
					jsonReply(w, http.StatusOK, `{"code":10014,"msg":"app secret invalid"}`)
					return
				}
				rejected.Add(1)
				jsonReply(w, http.StatusBadRequest, `{"code":99991663,"msg":"invalid access token"}`)
			}))
			t.Cleanup(server.Close)
			t.Cleanup(func() {
				if rejected.Load() != 1 || tokens.Load() != 2 {
					t.Errorf("Feishu rejected %d API requests after %d token requests; want 1 after 2", rejected.Load(), tokens.Load())
				}
			})
			return channelAt(t, server.URL, server.Client()), &posted
		},
		// The SDK refuses the call before sending anything.
		"no app id": func(t *testing.T) (*Channel, *atomic.Int32) {
			var posted atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				posted.Add(1)
				jsonReply(w, http.StatusOK, tokenOK)
			}))
			t.Cleanup(server.Close)
			return &Channel{api: apiAt("", "test-secret", server.URL, server.Client())}, &posted
		},
	}
	for cause, serve := range causes {
		for name, call := range outboundCalls {
			t.Run(cause+"/"+name, func(t *testing.T) {
				c, posted := serve(t)
				err := call(t.Context(), c)
				if err == nil {
					t.Fatal("a call Feishu never took succeeded")
				}
				if posted.Load() != 0 {
					t.Fatalf("the API request was posted %d times", posted.Load())
				}
				if errors.Is(err, channel.ErrOutcomeUnknown) || errors.Is(messageOutcome(err), channel.ErrOutcomeUnknown) {
					t.Fatalf("a request Feishu never took is reported as possibly delivered: %v", err)
				}
			})
		}
	}
}

// A CodeError from the SDK means Feishu never took the request: the token
// it needed was refused, or the SDK's own checks stopped it. A refused
// token request returns it as a value; the client-assertion checks return
// it as a pointer. Steve authenticates with the app secret and never
// reaches those checks, so the pointer form is tested here directly.
func TestAnSDKCodeErrorIsDefiniteInEitherForm(t *testing.T) {
	for _, cause := range []error{
		larkcore.CodeError{Code: 10014, Msg: "app secret invalid"},
		&larkcore.CodeError{Code: larkcore.ErrCodeClientAssertionRetrieveFailed, Msg: "retrieve failed"},
	} {
		c := &call{}
		c.token.Store(true)
		c.api.Store(true)
		err := c.failed("feishu send", fmt.Errorf("wrapped: %w", cause))
		if errors.Is(err, channel.ErrOutcomeUnknown) {
			t.Errorf("SDK code error %T is reported as possibly delivered: %v", cause, err)
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
