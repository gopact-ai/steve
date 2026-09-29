package feishu

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

// bootstrap is how the long-connection endpoint answers one request for a
// connection address: an HTTP status and a business code.
type bootstrap struct {
	status int
	code   int
	msg    string
	// attempts limits how often the official client tries to connect
	// again after it loses the connection this answer leads to; 0 is no
	// limit.
	attempts int
}

var (
	bootstrapOK = bootstrap{status: http.StatusOK}
	// bootstrapOKOnce has the official client try once after a loss.
	bootstrapOKOnce = bootstrap{status: http.StatusOK, attempts: 1}
	bootstrapBusy   = bootstrap{status: http.StatusServiceUnavailable, msg: "system busy"}
	// bootstrapRefused is a code the official client does not retry.
	bootstrapRefused = bootstrap{status: http.StatusOK, code: 403, msg: "application disabled"}
)

// longConnFeishu is a Feishu long-connection endpoint. It answers requests
// for a connection address in the order queued, repeating the last, and
// hands each connection it accepts to the test.
type longConnFeishu struct {
	server   *httptest.Server
	mu       sync.Mutex
	answers  []bootstrap
	accepted chan net.Conn
}

func newLongConnFeishu(t *testing.T, answers ...bootstrap) *longConnFeishu {
	t.Helper()
	f := &longConnFeishu{answers: answers, accepted: make(chan net.Conn, 8)}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(func() {
		f.server.Close()
		close(f.accepted)
		for conn := range f.accepted {
			_ = conn.Close()
		}
	})
	return f
}

func (f *longConnFeishu) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/callback/ws/endpoint":
		f.mu.Lock()
		answer := f.answers[0]
		if len(f.answers) > 1 {
			f.answers = f.answers[1:]
		}
		f.mu.Unlock()
		body := map[string]any{"code": answer.code, "msg": answer.msg}
		if answer.status == http.StatusOK && answer.code == 0 {
			// The official client takes its reconnect policy from here:
			// retry at once after a loss, a second apart, by default forever.
			attempts := -1
			if answer.attempts > 0 {
				attempts = answer.attempts
			}
			body["data"] = map[string]any{
				"URL":          "ws" + strings.TrimPrefix(f.server.URL, "http") + "/ws?device_id=d&service_id=1",
				"ClientConfig": map[string]int{"ReconnectCount": attempts, "ReconnectInterval": 1, "ReconnectNonce": 0, "PingInterval": 120},
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(answer.status)
		_ = json.NewEncoder(w).Encode(body)
	case "/ws":
		sum := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		_, _ = fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(sum[:]))
		_ = rw.Flush()
		f.accepted <- conn
	default:
		http.NotFound(w, r)
	}
}

// next waits for the next connection the endpoint accepts.
func (f *longConnFeishu) next(t *testing.T) net.Conn {
	t.Helper()
	select {
	case conn := <-f.accepted:
		return conn
	case <-time.After(waitDeadline):
		t.Fatal("the long connection was not established")
		return nil
	}
}

// lose drops conn as a network failure would, once the official client has
// sent its first ping on it. The client logs that ping outside its lock while
// a loss rewrites what it logs under the lock; losing the connection only
// after the ping keeps the race detector on this package's code.
func lose(t *testing.T, conn net.Conn) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(waitDeadline))
	if _, err := conn.Read(make([]byte, 1)); err != nil {
		t.Fatalf("no ping on the long connection: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	_ = conn.Close()
}

// reconnectReports records what a channel reports about its connection.
type reconnectReports struct {
	mu      sync.Mutex
	reports []*Reconnect
}

func (r *reconnectReports) add(report *Reconnect) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if report != nil {
		copied := *report
		report = &copied
	}
	r.reports = append(r.reports, report)
}

func (r *reconnectReports) snapshot() []*Reconnect {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*Reconnect(nil), r.reports...)
}

// waitFor waits until n reports have arrived.
func (r *reconnectReports) waitFor(t *testing.T, n int) []*Reconnect {
	t.Helper()
	deadline := time.Now().Add(waitDeadline)
	for {
		got := r.snapshot()
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("got %d connection reports %s; want %d", len(got), describeReports(got), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func describeReports(reports []*Reconnect) string {
	parts := make([]string, 0, len(reports))
	for _, r := range reports {
		if r == nil {
			parts = append(parts, "connected")
			continue
		}
		parts = append(parts, fmt.Sprintf("{failures %d since %s last attempt %s err %v}", r.Failures, r.Since.Format(time.RFC3339Nano), r.LastAttempt.Format(time.RFC3339Nano), r.Err))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// longConnChannel is a verified channel whose long connection goes to f.
func longConnChannel(t *testing.T, f *longConnFeishu) (*Channel, *reconnectReports) {
	t.Helper()
	reports := &reconnectReports{}
	c := &Channel{identify: knownBot, ready: make(chan struct{}), onReconnect: reports.add}
	c.ws = newLongConn(c, t.Name(), "test-secret", f.server.URL, dispatcher.NewEventDispatcher("", ""))
	return c, reports
}

// A lost long connection is reported while the official client establishes
// it again, with each failed attempt and when it failed, and withdrawn once
// it is back. A later loss is reported afresh.
func TestLongConnectionLossIsReportedUntilItIsBack(t *testing.T) {
	f := newLongConnFeishu(t, bootstrapOK, bootstrapBusy, bootstrapOKOnce, bootstrapBusy)
	c, reports := longConnChannel(t, f)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- c.Start(ctx) }()

	first := f.next(t)
	if got := reports.snapshot(); len(got) != 0 {
		t.Fatalf("a connected channel reported %s", describeReports(got))
	}
	lost := time.Now()
	lose(t, first)
	second := f.next(t)
	got := reports.waitFor(t, 3)
	if len(got) != 3 || got[0] == nil || got[1] == nil || got[2] != nil {
		t.Fatalf("reports %s; want the loss, the failed attempt, then connected", describeReports(got))
	}
	if got[0].Failures != 0 || got[0].Err != nil || !got[0].LastAttempt.IsZero() || got[0].Since.Before(lost) {
		t.Fatalf("the loss was reported as %s", describeReports(got[:1]))
	}
	if got[1].Failures != 1 || got[1].Since != got[0].Since || got[1].LastAttempt.Before(got[1].Since) ||
		got[1].Err == nil || !strings.Contains(got[1].Err.Error(), "system busy") {
		t.Fatalf("the failed attempt was reported as %s", describeReports(got[1:2]))
	}

	lostAgain := time.Now()
	_ = second.Close()
	got = reports.waitFor(t, 5)
	if len(got) != 5 || got[3] == nil || got[4] == nil || got[3].Failures != 0 || !got[3].LastAttempt.IsZero() || got[3].Since.Before(lostAgain) ||
		got[4].Failures != 1 || got[4].Since != got[3].Since || got[4].LastAttempt.Before(got[4].Since) ||
		got[4].Err == nil || !strings.Contains(got[4].Err.Error(), "system busy") {
		t.Fatalf("reports %s; want the second loss, then its failed attempt", describeReports(got))
	}

	// Closing the official client with a connection open races its own
	// logging, so the channel stops once the client has lost the connection
	// again and made its one attempt. TestStoppingTheChannelReportsNothingMore
	// stops a channel with its connection open.
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Start() = %v, want context.Canceled", err)
		}
	case <-time.After(waitDeadline):
		t.Fatal("Start did not return after cancel")
	}
}

// A lost long connection that Feishu then refuses is not retried by the
// official client; Start ends with the refusal instead of waiting on a
// connection that will not come back.
func TestLongConnectionRefusedAfterALossEndsStart(t *testing.T) {
	f := newLongConnFeishu(t, bootstrapOK, bootstrapRefused)
	c, _ := longConnChannel(t, f)
	errCh := make(chan error, 1)
	go func() { errCh <- c.Start(t.Context()) }()

	lose(t, f.next(t))
	select {
	case err := <-errCh:
		if err == nil || !strings.Contains(err.Error(), "application disabled") {
			t.Fatalf("Start() = %v, want the refusal", err)
		}
	case <-time.After(waitDeadline):
		t.Fatal("the channel kept waiting on a connection Feishu refused")
	}
}

// clientCallbacks are the watch's handlers for the official client's
// connection callbacks, as newLongConn installs them.
func clientCallbacks(w *connWatch) (disconnected, reconnecting func(), failed func(error), reconnected func()) {
	return w.lost, w.reconnecting, w.failed, w.back
}

// The official client reports a reconnect round as reconnecting, each
// failed attempt, then reconnected. A connection that drops as soon as it
// is made starts the next round before the round that made it reports
// reconnected; that late report does not end the round still in progress.
func TestALateReconnectedDoesNotEndTheNextRound(t *testing.T) {
	reports := &reconnectReports{}
	w := &connWatch{report: reports.add, refused: make(chan error, 1)}
	disconnected, reconnecting, failed, reconnected := clientCallbacks(w)

	disconnected()
	reconnecting()
	failed(errors.New("system busy"))
	// The first round connects, and the new connection drops at once.
	disconnected()
	reconnecting()
	failed(errors.New("system busy"))
	reconnected() // the first round, late
	got := reports.snapshot()
	if last := got[len(got)-1]; last == nil || last.Failures != 2 {
		t.Fatalf("reports %s; want the second round still reconnecting after two failures", describeReports(got))
	}
	reconnected()
	got = reports.snapshot()
	if got[len(got)-1] != nil {
		t.Fatalf("reports %s; want connected once the second round is back", describeReports(got))
	}
}

// Each failed attempt is reported with when it failed: the official client
// reports nothing when it stops trying, so a last attempt that stops
// advancing is how that shows. The loss, and the wait before the first
// attempt, have no attempt yet. A first connection after Ready that fails
// starts the reconnect with that attempt.
func TestEachFailedAttemptIsReportedWithWhenItFailed(t *testing.T) {
	reports := &reconnectReports{}
	w := &connWatch{report: reports.add, refused: make(chan error, 1)}
	disconnected, reconnecting, failed, _ := clientCallbacks(w)

	disconnected()
	reconnecting()
	if got := reports.snapshot(); len(got) != 1 || got[0] == nil || !got[0].LastAttempt.IsZero() {
		t.Fatalf("the loss was reported as %s; want no attempt yet", describeReports(got))
	}
	for attempt := 1; attempt <= 2; attempt++ {
		before := time.Now()
		failed(errors.New("system busy"))
		got := reports.snapshot()
		if last := got[len(got)-1]; last == nil || last.Failures != attempt || last.LastAttempt.Before(before) {
			t.Fatalf("after failed attempt %d made after %s: reports %s", attempt, before.Format(time.RFC3339Nano), describeReports(got))
		}
	}

	reports = &reconnectReports{}
	w = &connWatch{report: reports.add, refused: make(chan error, 1)}
	_, reconnecting, failed, _ = clientCallbacks(w)
	failed(errors.New("system busy"))
	reconnecting()
	got := reports.snapshot()
	if len(got) != 1 || got[0] == nil || got[0].Failures != 1 || got[0].LastAttempt.IsZero() || !got[0].LastAttempt.Equal(got[0].Since) {
		t.Fatalf("the failed first connection was reported as %s; want one failure, since that attempt", describeReports(got))
	}
}

// Once the channel stops, nothing the official client calls back is
// reported: the client keeps calling back after it is closed.
func TestAStoppedWatchReportsNothing(t *testing.T) {
	reports := &reconnectReports{}
	w := &connWatch{report: reports.add, refused: make(chan error, 1)}
	disconnected, reconnecting, failed, reconnected := clientCallbacks(w)

	disconnected()
	reconnecting()
	w.stop()
	failed(errors.New("system busy"))
	reconnected()
	disconnected()
	reconnecting()
	if got := reports.snapshot(); len(got) != 1 {
		t.Fatalf("a stopped watch reported %s", describeReports(got[1:]))
	}
}

// closingConn is a long connection that stays up until it is closed and
// calls back a loss as it closes, as the official client does when it is
// closed with a connection open. A refusal, if any, is called back once it
// starts, as a failure the client does not retry.
type closingConn struct {
	watch   *connWatch
	refusal error
	started chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (c *closingConn) Start(context.Context) error {
	close(c.started)
	if c.refusal != nil {
		c.watch.failed(c.refusal)
	}
	<-c.closed
	return nil
}

func (c *closingConn) Close() {
	c.once.Do(func() {
		c.watch.lost()
		close(c.closed)
	})
}

// Stopping the channel reports nothing more: whether it is cancelled or
// its connection is refused, the channel stops reporting before it closes
// the official client, whose Close calls back a loss.
func TestStoppingTheChannelReportsNothingMore(t *testing.T) {
	refusal := larkws.NewClientError(403, "application disabled")
	for _, tc := range []struct {
		name    string
		refusal error
		want    error
	}{
		{name: "cancelled", want: context.Canceled},
		{name: "refused", refusal: refusal, want: refusal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reports := &reconnectReports{}
			watch := &connWatch{report: reports.add, refused: make(chan error, 1)}
			conn := &closingConn{watch: watch, refusal: tc.refusal, started: make(chan struct{}), closed: make(chan struct{})}
			c := startingChannel(conn, knownBot)
			c.watch = watch
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			errCh := make(chan error, 1)
			go func() { errCh <- c.Start(ctx) }()

			select {
			case <-conn.started:
			case <-time.After(waitDeadline):
				t.Fatal("the long connection was not started")
			}
			if tc.refusal == nil {
				cancel()
			}
			select {
			case err := <-errCh:
				if !errors.Is(err, tc.want) {
					t.Fatalf("Start() = %v, want %v", err, tc.want)
				}
			case <-time.After(waitDeadline):
				t.Fatal("Start did not return")
			}
			select {
			case <-conn.closed:
			default:
				t.Fatal("the stopped channel left its long connection open")
			}
			if got := reports.snapshot(); len(got) != 0 {
				t.Fatalf("a stopped channel reported %s", describeReports(got))
			}
		})
	}
}
