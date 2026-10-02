package node

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func reverseRawServer(t *testing.T, handle func(net.Conn, *http.Request)) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer connection.Close()
		connection.SetDeadline(time.Now().Add(5 * time.Second))
		request, err := http.ReadRequest(bufio.NewReader(connection))
		if err != nil {
			t.Error(err)
			return
		}
		handle(connection, request)
	}()
	t.Cleanup(func() {
		listener.Close()
		select {
		case <-done:
		case <-time.After(6 * time.Second):
			t.Error("raw response fixture did not stop")
		}
	})
	return listener.Addr().String()
}

func reverseRequest(t *testing.T, payload string) *http.Request {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	t.Cleanup(cancel)
	ctx = context.WithValue(ctx, reverseReadAbortKey{}, reverseReadAbort(func() error { return nil }))
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://hub/mcp", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func TestReverseHTTPRejectsTruncatedResponses(t *testing.T) {
	for _, reply := range []string{
		"HTTP/1.1 401 Unauthorized\r\nContent-Len",
		"HTTP/1.1 401 Unauthorized\r\nContent-Length: 25\r\n\r\nshort",
		"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\na\r\nshort",
	} {
		t.Run(fmt.Sprint(len(reply)), func(t *testing.T) {
			address := reverseRawServer(t, func(connection net.Conn, request *http.Request) {
				io.Copy(io.Discard, request.Body)
				io.WriteString(connection, reply)
			})
			transport := newReverseHTTP(reverseHTTPDialer(t, address))
			response, err := transport.RoundTrip(reverseRequest(t, ""))
			if err == nil {
				_, err = io.ReadAll(response.Body)
				response.Body.Close()
			}
			if err == nil {
				t.Fatal("truncated response was reported complete")
			}
		})
	}
}

func TestReverseHTTPNeverReplaysAnAcceptedTool(t *testing.T) {
	var effects, dials atomic.Int32
	address := reverseRawServer(t, func(connection net.Conn, request *http.Request) {
		if _, err := io.ReadAll(request.Body); err != nil {
			t.Error(err)
			return
		}
		effects.Add(1)
		// The effect happened, but the connection ends without a response.
	})
	dial := reverseHTTPDialer(t, address)
	transport := newReverseHTTP(func(ctx context.Context) (reverseHTTPConnection, error) {
		dials.Add(1)
		return dial(ctx)
	})
	response, err := transport.RoundTrip(reverseRequest(t, strings.Repeat("body", 1<<15)))
	if response != nil {
		response.Body.Close()
	}
	if err == nil || effects.Load() != 1 || dials.Load() != 1 {
		t.Fatalf("lost reply: effects=%d streams=%d error=%v", effects.Load(), dials.Load(), err)
	}
}

func TestReverseHTTPHeaderBudgetDoesNotLimitTheBody(t *testing.T) {
	payload := strings.Repeat("x", reverseHeaderLimit+100)
	cases := []struct {
		name, reply string
		valid       bool
	}{
		{"large body", fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n%s", len(payload), payload), true},
		{"large header", "HTTP/1.1 200 OK\r\nX-Large: " + payload + "\r\nContent-Length: 0\r\n\r\n", false},
		{"eight informational", strings.Repeat("HTTP/1.1 100 Continue\r\n\r\n", 8) + "HTTP/1.1 204 No Content\r\n\r\n", true},
		{"too many informational", strings.Repeat("HTTP/1.1 100 Continue\r\n\r\n", 9) + "HTTP/1.1 204 No Content\r\n\r\n", false},
		{"switch protocol", "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: other\r\n\r\n", false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			response, err := readReverseResponse(strings.NewReader(test.reply), &http.Request{Method: http.MethodPost})
			if !test.valid {
				if err == nil {
					response.Body.Close()
					t.Fatal("invalid response headers accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || test.name == "large body" && string(body) != payload {
				t.Fatalf("valid response lost body: size=%d err=%v", len(body), err)
			}
		})
	}
}

func TestReverseHTTPRequiresReadInterruptionBeforeOpeningAStream(t *testing.T) {
	var opened bool
	transport := newReverseHTTP(func(context.Context) (reverseHTTPConnection, error) {
		opened = true
		return nil, errors.New("must not dial")
	})
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://hub/mcp", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transport.RoundTrip(request); err == nil || opened {
		t.Fatalf("request without read control: opened=%v err=%v", opened, err)
	}
}
