package node

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type noReadDeadline struct {
	net.Conn
	closed atomic.Bool
}

func (*noReadDeadline) SetReadDeadline(time.Time) error { return http.ErrNotSupported }
func (c *noReadDeadline) Close() error {
	c.closed.Store(true)
	return c.Conn.Close()
}

func TestReverseHTTPFailedReadInterruptClosesTheOwnedConnection(t *testing.T) {
	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()
	connection := &noReadDeadline{Conn: local}
	ctx := reverseConnectionContext(t.Context(), connection)
	abort := ctx.Value(reverseReadAbortKey{}).(reverseReadAbort)
	if err := abort(); !errors.Is(err, http.ErrNotSupported) || !connection.closed.Load() {
		t.Fatalf("failed interrupt left its connection open: closed=%v err=%v", connection.closed.Load(), err)
	}
	if _, err := remote.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("closed interrupt peer = %v, want EOF", err)
	}
}

func TestReverseMCPEndpointWithoutControlsFailsBeforeDial(t *testing.T) {
	for _, withReadAbort := range []bool{false, true} {
		server := &Server{}
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		request := httptest.NewRequest(http.MethodPost, "http://node/mcp", nil).WithContext(ctx)
		if withReadAbort {
			ctx := context.WithValue(request.Context(), reverseReadAbortKey{}, reverseReadAbort(func() error { return nil }))
			request = request.WithContext(ctx)
		}
		response := httptest.NewRecorder()
		server.reverseMCPHandler().ServeHTTP(response, request)
		if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "1" || !strings.Contains(response.Body.String(), "hub unreachable") {
			t.Fatalf("missing controls response=%d %s", response.Code, response.Body.String())
		}
		if server.hubWaiters != nil {
			t.Fatal("a request without controls tried to connect upstream")
		}
	}
}

type unwrapReverseWriter struct{ http.ResponseWriter }

func (w unwrapReverseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func TestReverseHTTPPreparationSupportsStandardUnwrap(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := prepareReverseHTTP(unwrapReverseWriter{w}, r); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	server.Config.ConnContext = reverseConnectionContext
	server.Start()
	t.Cleanup(server.Close)
	response, err := server.Client().Post(server.URL, "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent || !response.Close {
		t.Fatalf("wrapped controls response=%d close=%v", response.StatusCode, response.Close)
	}
}
