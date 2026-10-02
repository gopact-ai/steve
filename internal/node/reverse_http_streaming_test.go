package node

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func reverseEndpoint(t *testing.T, address string) string {
	t.Helper()
	worker := startNode(t, ServerConfig{Name: "worker", Token: "token", StateDir: t.TempDir()})
	registry := NewRegistry("hub", map[string]Config{"worker": {Addr: worker.Addr(), Token: "token"}})
	t.Cleanup(registry.Close)
	registry.SetMCPDialer(func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", address)
	})
	endpoint, err := registry.MCPEndpoint(t.Context(), "worker")
	if err != nil {
		t.Fatal(err)
	}
	return endpoint
}

func unfinishedReverseUpload(t *testing.T, endpoint string, chunked bool) net.Conn {
	t.Helper()
	address := strings.TrimSuffix(strings.TrimPrefix(endpoint, "http://"), "/mcp")
	connection, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { connection.Close() })
	if err := connection.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("x", 128<<10)
	head := "POST /mcp HTTP/1.1\r\nHost: worker\r\n"
	if chunked {
		head += "Transfer-Encoding: chunked\r\n\r\n"
		body = fmt.Sprintf("%x\r\n%s\r\n", len(body), body)
	} else {
		head += "Content-Length: 1048576\r\n\r\n"
	}
	if _, err := io.WriteString(connection, head+body); err != nil {
		t.Fatal(err)
	}
	return connection
}

func TestReverseMCPEarlyFinalStreamsItsWholeResponse(t *testing.T) {
	for _, chunked := range []bool{false, true} {
		t.Run(fmt.Sprint("chunked-upload-", chunked), func(t *testing.T) {
			want := strings.Repeat("response-data\n", 100000)
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var first [1]byte
				if _, err := io.ReadFull(r.Body, first[:]); err != nil {
					return
				}
				w.Header().Set("Content-Type", "text/plain")
				w.WriteHeader(http.StatusConflict)
				for offset := 0; offset < len(want); offset += 8192 {
					end := min(offset+8192, len(want))
					if _, err := io.WriteString(w, want[offset:end]); err != nil {
						return
					}
					w.(http.Flusher).Flush()
				}
			}))
			t.Cleanup(upstream.Close)
			endpoint := reverseEndpoint(t, strings.TrimPrefix(upstream.URL, "http://"))
			connection := unfinishedReverseUpload(t, endpoint, chunked)
			response, err := http.ReadResponse(bufio.NewReader(connection), &http.Request{Method: http.MethodPost})
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || response.StatusCode != http.StatusConflict || string(got) != want {
				t.Fatalf("early response status=%d bytes=%d want=%d error=%v", response.StatusCode, len(got), len(want), err)
			}
			if calls.Load() != 1 {
				t.Fatalf("one request ran %d times", calls.Load())
			}
		})
	}
}

func TestReverseMCPEarlyStreamingResponseLivesUntilClientCloses(t *testing.T) {
	continueResponse := make(chan struct{})
	finished := make(chan struct{})
	stop := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(finished)
		if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
			t.Error(err)
			return
		}
		drained := make(chan struct{})
		go func() { io.Copy(io.Discard, r.Body); close(drained) }()
		defer func() {
			select {
			case <-drained:
			case <-time.After(2 * time.Second):
				t.Error("upstream upload reader did not finish")
			}
		}()
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-continueResponse:
		case <-r.Context().Done():
			return
		case <-stop:
			http.NewResponseController(w).SetReadDeadline(time.Now())
			return
		}
		io.WriteString(w, "data: second\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-stop:
			http.NewResponseController(w).SetReadDeadline(time.Now())
		}
	}))
	t.Cleanup(func() { close(stop); upstream.Close() })
	endpoint := reverseEndpoint(t, strings.TrimPrefix(upstream.URL, "http://"))
	connection := unfinishedReverseUpload(t, endpoint, true)
	response, err := http.ReadResponse(bufio.NewReader(connection), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(response.Body)
	for index, want := range []string{"data: first\n", "\n", "data: second\n", "\n"} {
		line, err := reader.ReadString('\n')
		if err != nil || line != want {
			t.Fatalf("event %d = %q: %v", index, line, err)
		}
		if index == 1 {
			close(continueResponse)
		}
	}
	connection.Close()
	response.Body.Close()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("closing the client did not release the upstream event stream")
	}
}
