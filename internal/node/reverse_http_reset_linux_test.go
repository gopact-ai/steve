//go:build linux

package node

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// This upstream intentionally closes while its declared upload is unfinished.
// Linux may discard unsent response bytes when that close resets the TCP peer.
// Hold a real response read until the real server close, not a fabricated EOF.
func TestReverseMCPEarlyResetIsNotACompleteResponse(t *testing.T) {
	want := strings.Repeat("response-data\n", 100000)
	closed := make(chan struct{})
	var once sync.Once
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
		w.WriteHeader(http.StatusConflict)
		for offset := 0; offset < len(want); offset += 8192 {
			end := min(offset+8192, len(want))
			n, err := io.WriteString(w, want[offset:end])
			written.Add(int64(n))
			if err != nil {
				t.Error(err)
				return
			}
			w.(http.Flusher).Flush()
		}
	}))
	upstream.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			once.Do(func() { close(closed) })
		}
	}
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
		return &readAfterUpstreamClose{Conn: connection, closed: closed, reset: &reset}, nil
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
	if written.Load() != int64(len(want)) || calls.Load() != 1 || dials.Load() != 1 {
		t.Fatalf("reset fixture or request replay: written=%d calls=%d dials=%d", written.Load(), calls.Load(), dials.Load())
	}
}

type readAfterUpstreamClose struct {
	net.Conn
	closed <-chan struct{}
	reset  *atomic.Bool
	read   int
	paused bool
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
	if errors.Is(err, syscall.ECONNRESET) {
		c.reset.Store(true)
	}
	return n, err
}
