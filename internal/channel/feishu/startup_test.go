package feishu

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
)

const (
	tokenPath   = "/open-apis/auth/v3/tenant_access_token/internal"
	botInfoPath = "/open-apis/bot/v3/info"
	tokenOK     = `{"code":0,"tenant_access_token":"test-token","expire":7200}`
)

// startupOutcome is what one Start did against a Feishu that answers every
// verification with the same failure.
type startupOutcome struct {
	attempts int32
	err      error
	started  bool
}

// startAgainst starts a channel whose verification reads the bot identity
// from a test Feishu served by handler. It stops the channel at its second
// verification, so a retried failure is observed without waiting for it.
func startAgainst(t *testing.T, secret string, handler http.HandlerFunc) startupOutcome {
	t.Helper()
	server := httptest.NewServer(handler)
	defer server.Close()
	api := lark.NewClient(t.Name(), secret, lark.WithOpenBaseUrl(server.URL), lark.WithHttpClient(server.Client()))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var attempts atomic.Int32
	conn := newStuckConn()
	c := startingChannel(conn, func(ctx context.Context) (Identity, error) {
		if attempts.Add(1) > 1 {
			cancel()
		}
		return botIdentity(ctx, api)
	})
	errCh := make(chan error, 1)
	go func() { errCh <- c.Start(ctx) }()
	select {
	case err := <-errCh:
		started := false
		select {
		case <-conn.started:
			started = true
		default:
		}
		return startupOutcome{attempts: attempts.Load(), err: err, started: started}
	case <-time.After(waitDeadline):
		t.Fatal("Start neither retried nor returned")
		return startupOutcome{}
	}
}

func jsonReply(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// botInfo answers the token request and replies to the identity read with
// status and body.
func botInfo(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == tokenPath {
			jsonReply(w, http.StatusOK, tokenOK)
			return
		}
		jsonReply(w, status, body)
	}
}

// token replies to the token request with status and body.
func token(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != tokenPath {
			t := "unexpected request " + r.URL.Path
			http.Error(w, t, http.StatusTeapot)
			return
		}
		jsonReply(w, status, body)
	}
}

// Credentials, configuration and application state Feishu rejects do not
// change by waiting: Start reports them once instead of retrying.
func TestStartReportsAPermanentStartupFailureWithoutRetrying(t *testing.T) {
	for name, tc := range map[string]struct {
		secret  string
		handler http.HandlerFunc
	}{
		"invalid secret":     {"wrong-secret", token(http.StatusOK, `{"code":10014,"msg":"app secret invalid"}`)},
		"missing secret":     {"", token(http.StatusOK, tokenOK)},
		"forbidden":          {"test-secret", botInfo(http.StatusForbidden, `{"code":99991672,"msg":"access denied"}`)},
		"bot not enabled":    {"test-secret", botInfo(http.StatusOK, `{"code":0,"bot":{}}`)},
		"rejected bot query": {"test-secret", botInfo(http.StatusOK, `{"code":10002,"msg":"invalid app"}`)},
	} {
		t.Run(name, func(t *testing.T) {
			got := startAgainst(t, tc.secret, tc.handler)
			if got.attempts != 1 || got.err == nil || errors.Is(got.err, context.Canceled) || got.started {
				t.Fatalf("permanent failure: %d attempts, err %v, connected %t; want one attempt reported as the error", got.attempts, got.err, got.started)
			}
		})
	}
}

// Network failures, timeouts, server errors and rate limits pass: Start
// tries again.
func TestStartRetriesATransientStartupFailure(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"unavailable": botInfo(http.StatusServiceUnavailable, `{"code":0}`),
		"bad gateway page": func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "<html>bad gateway</html>", http.StatusBadGateway)
		},
		"gateway timeout":    botInfo(http.StatusGatewayTimeout, `{}`),
		"too many requests":  botInfo(http.StatusTooManyRequests, `{"code":99991400,"msg":"request trigger frequency limit"}`),
		"token rate limited": token(http.StatusOK, `{"code":99991400,"msg":"request trigger frequency limit"}`),
		"connection dropped": func(w http.ResponseWriter, _ *http.Request) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := startAgainst(t, "test-secret", handler)
			if got.attempts != 2 || !errors.Is(got.err, context.Canceled) || got.started {
				t.Fatalf("transient failure: %d attempts, err %v, connected %t; want a second attempt", got.attempts, got.err, got.started)
			}
		})
	}
}

// Retry waits double from the base up to the cap, never exceed it, and keep
// at least half of their step so jitter cannot collapse them into a spin.
func TestStartupRetryDelayIsBoundedExponentialWithJitter(t *testing.T) {
	for failures, step := range map[int]time.Duration{
		1: startupRetryBase, 2: 2 * startupRetryBase, 3: 4 * startupRetryBase,
		8: startupRetryMax, 64: startupRetryMax, 1 << 20: startupRetryMax,
	} {
		for range 200 {
			if d := startupRetryDelay(failures); d < step/2 || d > step {
				t.Fatalf("delay after %d failures = %s; want within [%s, %s]", failures, d, step/2, step)
			}
		}
	}
}
