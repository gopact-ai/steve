//go:build linux

package node

import (
	"bufio"
	"context"
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
	"time"
)

// Reset the real socket before the declared response is complete. Closing an
// ordinary HTTP handler with an unfinished upload does not reliably reset it:
// a fully received response remains valid even if the upload did not finish.
func TestReverseMCPEarlyResetIsNotACompleteResponse(t *testing.T) {
	want := strings.Repeat("response-data\n", 100000)
	prefix := want[:96<<10]
	closed := make(chan struct{})
	received := make(chan struct{})
	var calls, dials atomic.Int32
	var written atomic.Int64
	var reset atomic.Bool
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var first [1]byte
		if _, err := io.ReadFull(r.Body, first[:]); err != nil {
			t.Error(err)
			return
		}
		connection, writer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer func() {
			connection.Close()
			close(closed)
		}()
		if err := connection.(*net.TCPConn).SetLinger(0); err != nil {
			t.Error(err)
			return
		}
		if _, err := fmt.Fprintf(writer, "HTTP/1.1 409 Conflict\r\nContent-Length: %d\r\nConnection: close\r\n\r\n", len(want)); err != nil {
			t.Error(err)
			return
		}
		n, err := io.WriteString(writer, prefix)
		written.Add(int64(n))
		if err != nil {
			t.Error(err)
			return
		}
		if err := writer.Flush(); err != nil {
			t.Error(err)
			return
		}
		select {
		case <-received:
		case <-time.After(3 * time.Second):
			t.Error("response prefix was not received before reset")
		}
	}))
	upstream.Start()
	t.Cleanup(upstream.Close)
	worker := startNode(t, ServerConfig{Name: "worker", Token: "token", StateDir: t.TempDir()})
	registry := NewRegistry("hub", map[string]Config{"worker": {Addr: worker.Addr(), Token: "token"}})
	t.Cleanup(registry.Close)
	registry.SetMCPDialer(func(ctx context.Context) (net.Conn, error) {
		dials.Add(1)
		connection, err := (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(upstream.URL, "http://"))
		if err != nil {
			return nil, err
		}
		if err := connection.(*net.TCPConn).SetReadBuffer(32 << 10); err != nil {
			connection.Close()
			return nil, err
		}
		return &readAfterUpstreamClose{Conn: connection, closed: closed, received: received, reset: &reset}, nil
	})
	endpoint, err := registry.MCPEndpoint(t.Context(), "worker")
	if err != nil {
		t.Fatal(err)
	}
	client := unfinishedReverseUpload(t, endpoint, false)
	response, err := http.ReadResponse(bufio.NewReader(client), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err == nil || response.StatusCode != http.StatusConflict || len(body) >= len(want) || !reset.Load() {
		t.Fatalf("upstream reset accepted: status=%d bytes=%d want=%d TCP-reset=%v error=%v", response.StatusCode, len(body), len(want), reset.Load(), err)
	}
	if written.Load() != int64(len(prefix)) || calls.Load() != 1 || dials.Load() != 1 {
		t.Fatalf("reset fixture or request replay: written=%d calls=%d dials=%d", written.Load(), calls.Load(), dials.Load())
	}
}

type readAfterUpstreamClose struct {
	net.Conn
	closed       <-chan struct{}
	received     chan struct{}
	reset        *atomic.Bool
	read         int
	paused       bool
	acknowledged bool
}

func (c *readAfterUpstreamClose) Read(p []byte) (int, error) {
	if c.read >= 64<<10 && !c.paused {
		c.paused = true
		select {
		case <-c.closed:
		case <-time.After(3 * time.Second):
			return 0, errors.New("upstream did not close its unfinished request")
		}
	}
	n, err := c.Conn.Read(p)
	c.read += n
	if c.read >= 64<<10 && !c.acknowledged {
		c.acknowledged = true
		close(c.received)
	}
	if errors.Is(err, syscall.ECONNRESET) {
		c.reset.Store(true)
	}
	return n, err
}

func (c *readAfterUpstreamClose) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if errors.Is(err, syscall.ECONNRESET) {
		c.reset.Store(true)
	}
	return n, err
}
