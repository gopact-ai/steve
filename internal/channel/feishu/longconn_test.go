package feishu

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
)

// bootstrap is how the long-connection endpoint answers one request for a
// connection address: an HTTP status and a business code.
type bootstrap struct {
	status int
	code   int
	msg    string
}

var (
	bootstrapOK   = bootstrap{status: http.StatusOK}
	bootstrapBusy = bootstrap{status: http.StatusServiceUnavailable, msg: "system busy"}
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
		if answer == bootstrapOK {
			// The official client takes its reconnect policy from here:
			// retry forever, at once after a loss, a second apart.
			body["data"] = map[string]any{
				"URL":          "ws" + strings.TrimPrefix(f.server.URL, "http") + "/ws?device_id=d&service_id=1",
				"ClientConfig": map[string]int{"ReconnectCount": -1, "ReconnectInterval": 1, "ReconnectNonce": 0, "PingInterval": 120},
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
		parts = append(parts, fmt.Sprintf("{failures %d since %s err %v}", r.Failures, r.Since.Format(time.RFC3339Nano), r.Err))
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
// it again, with each failed attempt, and withdrawn once it is back.
// Stopping the channel reports nothing more.
func TestLongConnectionLossIsReportedUntilItIsBack(t *testing.T) {
	f := newLongConnFeishu(t, bootstrapOK, bootstrapBusy, bootstrapOK)
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
	_ = first.Close()
	f.next(t)
	got := reports.waitFor(t, 3)
	if len(got) != 3 || got[0] == nil || got[1] == nil || got[2] != nil {
		t.Fatalf("reports %s; want the loss, the failed attempt, then connected", describeReports(got))
	}
	if got[0].Failures != 0 || got[0].Err != nil || got[0].Since.Before(lost) {
		t.Fatalf("the loss was reported as %s", describeReports(got[:1]))
	}
	if got[1].Failures != 1 || got[1].Since != got[0].Since || got[1].Err == nil || !strings.Contains(got[1].Err.Error(), "system busy") {
		t.Fatalf("the failed attempt was reported as %s", describeReports(got[1:2]))
	}

	cancel()
	select {
	case <-errCh:
	case <-time.After(waitDeadline):
		t.Fatal("Start did not return after cancel")
	}
	if after := reports.snapshot(); len(after) != 3 {
		t.Fatalf("a stopped channel reported %s", describeReports(after[3:]))
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

	_ = f.next(t).Close()
	select {
	case err := <-errCh:
		if err == nil || !strings.Contains(err.Error(), "application disabled") {
			t.Fatalf("Start() = %v, want the refusal", err)
		}
	case <-time.After(waitDeadline):
		t.Fatal("the channel kept waiting on a connection Feishu refused")
	}
}
