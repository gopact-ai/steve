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

func TestReverseMCPRequestBudgetCountsTheWholeBody(t *testing.T) {
	for _, chunked := range []bool{false, true} {
		for _, size := range []int{reverseRequestLimit, reverseRequestLimit + 1} {
			t.Run(fmt.Sprintf("chunked-%v-bytes-%d", chunked, size), func(t *testing.T) {
				var effects atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						return
					}
					effects.Add(1)
					fmt.Fprintf(w, "%d", len(body))
				}))
				t.Cleanup(upstream.Close)
				endpoint := reverseEndpoint(t, strings.TrimPrefix(upstream.URL, "http://"))
				var body io.Reader = strings.NewReader(strings.Repeat("x", size))
				if chunked {
					body = &reverseBody{body}
				}
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
				if err != nil {
					t.Fatal(err)
				}
				client := &http.Client{Timeout: 5 * time.Second}
				defer client.CloseIdleConnections()
				response, err := client.Do(request)
				if err != nil {
					t.Fatal(err)
				}
				result, err := io.ReadAll(response.Body)
				response.Body.Close()
				wantStatus, wantEffects := http.StatusOK, int32(1)
				if size > reverseRequestLimit {
					wantStatus, wantEffects = http.StatusRequestEntityTooLarge, 0
				}
				if err != nil || response.StatusCode != wantStatus || effects.Load() != wantEffects {
					t.Fatalf("budget status=%d effects=%d body=%s err=%v", response.StatusCode, effects.Load(), result, err)
				}
			})
		}
	}
}

func TestReverseMCPDeclaredExcessNeverOpensAnUpstreamRequest(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	endpoint := reverseEndpoint(t, strings.TrimPrefix(upstream.URL, "http://"))
	connection, err := net.Dial("tcp", strings.TrimSuffix(strings.TrimPrefix(endpoint, "http://"), "/mcp"))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	connection.SetDeadline(time.Now().Add(2 * time.Second))
	io.WriteString(connection, fmt.Sprintf("POST /mcp HTTP/1.1\r\nHost: worker\r\nContent-Length: %d\r\n\r\n", reverseRequestLimit+1))
	response, err := http.ReadResponse(bufio.NewReader(connection), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusRequestEntityTooLarge || calls.Load() != 0 {
		t.Fatalf("declared excess status=%d upstream=%d", response.StatusCode, calls.Load())
	}
}

func TestReverseMCPTailIsBoundedWithoutForwardingIt(t *testing.T) {
	for _, excess := range []bool{false, true} {
		t.Run(fmt.Sprint("excess-", excess), func(t *testing.T) {
			var received atomic.Int64
			finished := make(chan struct{})
			stop := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(finished)
				http.NewResponseController(w).EnableFullDuplex()
				first := make([]byte, 16)
				n, err := io.ReadFull(r.Body, first)
				received.Add(int64(n))
				if err != nil {
					return
				}
				drained := make(chan struct{})
				go func() { n, _ := io.Copy(io.Discard, r.Body); received.Add(n); close(drained) }()
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: rejected\n\n")
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
				case <-stop:
					http.NewResponseController(w).SetReadDeadline(time.Now())
				}
				select {
				case <-drained:
				case <-time.After(time.Second):
					t.Error("upstream body reader outlived response")
				}
			}))
			t.Cleanup(func() { close(stop); upstream.Close() })
			endpoint := reverseEndpoint(t, strings.TrimPrefix(upstream.URL, "http://"))
			connection, err := net.DialTimeout("tcp", strings.TrimSuffix(strings.TrimPrefix(endpoint, "http://"), "/mcp"), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			connection.SetDeadline(time.Now().Add(4 * time.Second))
			io.WriteString(connection, "POST /mcp HTTP/1.1\r\nHost: worker\r\nTransfer-Encoding: chunked\r\n\r\n10\r\nxxxxxxxxxxxxxxxx\r\n")
			response, err := http.ReadResponse(bufio.NewReader(connection), &http.Request{Method: http.MethodPost})
			if err != nil {
				t.Fatal(err)
			}
			first := make([]byte, len("data: rejected\n\n"))
			if _, err := io.ReadFull(response.Body, first); err != nil {
				t.Fatal(err)
			}
			size := reverseRequestLimit - 16
			if excess {
				size++
			}
			_, writeErr := io.WriteString(connection, fmt.Sprintf("%x\r\n%s\r\n0\r\n\r\n", size, strings.Repeat("x", size)))
			if !excess && writeErr != nil {
				t.Fatal(writeErr)
			}
			if excess {
				if _, err := io.ReadAll(response.Body); err == nil {
					t.Fatal("excess tail was reported as a complete streaming response")
				}
			} else {
				connection.Close()
			}
			response.Body.Close()
			select {
			case <-finished:
			case <-time.After(2 * time.Second):
				t.Fatal("tail termination did not release the upstream")
			}
			if got := received.Load(); got != 16 {
				t.Fatalf("tail reached upstream: received=%d, want original 16 bytes only", got)
			}
		})
	}
}
