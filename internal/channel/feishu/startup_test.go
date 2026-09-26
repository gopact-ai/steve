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
	// ready is whether Start reported the channel ready.
	ready bool
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
	var ready atomic.Bool
	conn := newStuckConn()
	c := startingChannel(conn, func(ctx context.Context) (Identity, error) {
		if attempts.Add(1) > 1 {
			cancel()
		}
		return botIdentity(ctx, api)
	})
	c.onReady = func() { ready.Store(true) }
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
		return startupOutcome{attempts: attempts.Load(), err: err, started: started, ready: ready.Load()}
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
			if got.attempts != 1 || got.err == nil || errors.Is(got.err, context.Canceled) || got.started || got.ready {
				t.Fatalf("permanent failure: %d attempts, err %v, connected %t, ready %t; want one attempt reported as the error", got.attempts, got.err, got.started, got.ready)
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
			if got.attempts != 2 || !errors.Is(got.err, context.Canceled) || got.started || got.ready {
				t.Fatalf("transient failure: %d attempts, err %v, connected %t, ready %t; want a second attempt", got.attempts, got.err, got.started, got.ready)
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
		seen := map[time.Duration]bool{}
		for range 200 {
			d := startupRetryDelay(failures)
			if d < step/2 || d > step {
				t.Fatalf("delay after %d failures = %s; want within [%s, %s]", failures, d, step/2, step)
			}
			seen[d] = true
		}
		if len(seen) < 2 {
			t.Fatalf("delay after %d failures is always %v; want jitter", failures, seen)
		}
	}
}

// unreachable is a verification failure Start retries.
func unreachable(context.Context) (Identity, error) {
	return Identity{}, errors.New("dial tcp: network is unreachable")
}

// Stopping the Hub while Start waits to retry ends the wait: Start returns
// at once, and no later attempt runs. Retrying happens on Start's own
// goroutine, so its return is the end of the retry.
func TestStopEndsAStartupRetryWait(t *testing.T) {
	var attempts atomic.Int32
	waiting := make(chan struct{})
	conn := newStuckConn()
	c := startingChannel(conn, func(ctx context.Context) (Identity, error) {
		attempts.Add(1)
		return unreachable(ctx)
	})
	c.delay = func(int) time.Duration {
		close(waiting)
		return time.Hour
	}
	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() { errCh <- c.Start(ctx) }()
	select {
	case <-waiting:
	case <-time.After(waitDeadline):
		t.Fatal("Start never scheduled a retry")
	}
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Start() = %v, want context.Canceled", err)
		}
	case <-time.After(waitDeadline):
		t.Fatal("stopping did not end the retry wait")
	}
	if attempts.Load() != 1 {
		t.Fatalf("%d attempts; want none after the stop", attempts.Load())
	}
	select {
	case <-conn.started:
		t.Fatal("a stopped channel connected")
	case <-c.Ready():
		t.Fatal("a stopped channel reported ready")
	default:
	}
}

// Connection settings apply by restarting the Hub, which stops the channel.
// A verification in flight at that moment is abandoned rather than awaited
// for its full timeout, and is not followed by another.
func TestStopAbandonsAStartupAttemptInFlight(t *testing.T) {
	var attempts atomic.Int32
	inFlight := make(chan struct{})
	c := startingChannel(newStuckConn(), func(ctx context.Context) (Identity, error) {
		if attempts.Add(1) == 1 {
			close(inFlight)
		}
		<-ctx.Done()
		return Identity{}, ctx.Err()
	})
	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() { errCh <- c.Start(ctx) }()
	select {
	case <-inFlight:
	case <-time.After(waitDeadline):
		t.Fatal("Start never verified")
	}
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Start() = %v, want context.Canceled", err)
		}
	case <-time.After(waitDeadline):
		t.Fatal("stopping did not abandon the verification in flight")
	}
	if attempts.Load() != 1 {
		t.Fatalf("%d attempts; want none after the stop", attempts.Load())
	}
}

// Every retried failure is reported before Start waits: how many attempts
// failed, when the next begins and why the last one failed.
func TestStartReportsEachStartupRetry(t *testing.T) {
	var attempts atomic.Int32
	conn := newStuckConn()
	c := startingChannel(conn, func(ctx context.Context) (Identity, error) {
		if attempts.Add(1) <= 2 {
			return unreachable(ctx)
		}
		return Identity{OpenID: "ou_bot"}, nil
	})
	c.delay = func(failures int) time.Duration { return time.Duration(failures) * time.Millisecond }
	var reports []StartRetry
	var reported []time.Time
	c.onRetry = func(r StartRetry) {
		reports = append(reports, r)
		reported = append(reported, time.Now())
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- c.Start(ctx) }()
	select {
	case <-conn.started:
	case err := <-errCh:
		t.Fatalf("Start() = %v before connecting", err)
	case <-time.After(waitDeadline):
		t.Fatal("the long connection never started")
	}
	if len(reports) != 2 {
		t.Fatalf("%d retries reported; want 2: %+v", len(reports), reports)
	}
	for i, r := range reports {
		delay := time.Duration(i+1) * time.Millisecond
		if r.Failures != i+1 || r.Err == nil || r.Err.Error() != "dial tcp: network is unreachable" ||
			r.Next.Before(reported[i]) || r.Next.After(reported[i].Add(delay)) {
			t.Fatalf("retry %d reported %+v at %s; want failure %d, its error and the next attempt within %s", i+1, r, reported[i], i+1, delay)
		}
	}
	cancel()
	<-errCh
}

// What serves Feishu work learns the channel is usable once, after its
// identity is verified and before anything waiting on Ready or the long
// connection's first event can observe the channel.
func TestStartReportsReadyAfterVerificationAndBeforeConnecting(t *testing.T) {
	var attempts atomic.Int32
	conn := newStuckConn()
	c := startingChannel(conn, func(ctx context.Context) (Identity, error) {
		if attempts.Add(1) == 1 {
			return unreachable(ctx)
		}
		return Identity{OpenID: "ou_bot"}, nil
	})
	var calls atomic.Int32
	var observed string
	c.onReady = func() {
		calls.Add(1)
		select {
		case <-conn.started:
			observed = "the long connection had started"
		case <-c.Ready():
			observed = "Ready was already closed"
		default:
			if c.botOpenID != "ou_bot" {
				observed = "the bot identity was not recorded"
			}
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- c.Start(ctx) }()
	select {
	case <-conn.started:
	case err := <-errCh:
		t.Fatalf("Start() = %v before connecting", err)
	case <-time.After(waitDeadline):
		t.Fatal("the long connection never started")
	}
	if calls.Load() != 1 || observed != "" || attempts.Load() != 2 {
		t.Fatalf("ready reported %d times after %d attempts (%s); want once, after the verified second attempt", calls.Load(), attempts.Load(), observed)
	}
	cancel()
	<-errCh
}
