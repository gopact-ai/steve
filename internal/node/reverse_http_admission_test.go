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

func TestReverseMCPSealedAdmissionReleasesUnfinishedUploads(t *testing.T) {
	for _, tc := range []struct {
		name, request string
		status        int
		body          string
	}{
		{"small", "Content-Length: 16\r\n\r\nx", http.StatusServiceUnavailable, "node is restarting"},
		{"chunked", "Transfer-Encoding: chunked\r\n\r\n10\r\nx", http.StatusServiceUnavailable, "node is restarting"},
		{"declared-excess", fmt.Sprintf("Content-Length: %d\r\n\r\n", reverseRequestLimit+1), http.StatusRequestEntityTooLarge, "exceeds the input limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, address, dispatched := sealedReverseEndpoint(t)
			client, err := net.DialTimeout("tcp", address, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			if err := client.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(client, "POST /mcp HTTP/1.1\r\nHost: worker\r\n"+tc.request); err != nil {
				t.Fatal(err)
			}
			reader := bufio.NewReader(client)
			response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodPost})
			if err != nil {
				t.Fatalf("sealed admission waited for the unfinished upload: %v", err)
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || response.StatusCode != tc.status || !strings.Contains(string(body), tc.body) || !response.Close {
				t.Fatalf("sealed rejection status=%d close=%v body=%q error=%v", response.StatusCode, response.Close, body, err)
			}
			if _, err := reader.ReadByte(); err != io.EOF {
				t.Fatalf("rejected connection remains readable: %v", err)
			}
			assertReverseAdmissionIdle(t, s, dispatched)
		})
	}
}

func TestReverseMCPSealedAdmissionDoesNotReuseTheConnection(t *testing.T) {
	s, address, dispatched := sealedReverseEndpoint(t)
	client, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	client.SetDeadline(time.Now().Add(2 * time.Second))
	request := "POST /mcp HTTP/1.1\r\nHost: worker\r\nContent-Length: 0\r\n\r\n"
	if _, err := io.WriteString(client, request+request); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(client)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable || !response.Close {
		t.Fatalf("sealed response status=%d close=%v", response.StatusCode, response.Close)
	}
	if _, err := reader.ReadByte(); err != io.EOF {
		t.Fatalf("sealed connection produced data for a second request: %v", err)
	}
	assertReverseAdmissionIdle(t, s, dispatched)
}

// The node has a reachable hub so a zero dispatch count proves the refusal
// happened before upstream work, rather than failing because no hub exists.
func sealedReverseEndpoint(t *testing.T) (*Server, string, *atomic.Int32) {
	t.Helper()
	worker := startNode(t, ServerConfig{Name: "worker", Token: "token", StateDir: t.TempDir()})
	registry := NewRegistry("hub", map[string]Config{"worker": {Addr: worker.Addr(), Token: "token"}})
	t.Cleanup(registry.Close)
	dispatched := &atomic.Int32{}
	registry.SetMCPDialer(func(context.Context) (net.Conn, error) {
		dispatched.Add(1)
		return nil, fmt.Errorf("sealed request reached the hub")
	})
	endpoint, err := registry.MCPEndpoint(t.Context(), "worker")
	if err != nil {
		t.Fatal(err)
	}
	worker.restart.mu.Lock()
	worker.restart.draining = true
	worker.restart.mu.Unlock()
	return worker, strings.TrimSuffix(strings.TrimPrefix(endpoint, "http://"), "/mcp"), dispatched
}

func assertReverseAdmissionIdle(t *testing.T, s *Server, dispatched *atomic.Int32) {
	t.Helper()
	s.restart.mu.Lock()
	active := s.restart.active
	s.restart.mu.Unlock()
	if active != 0 || dispatched.Load() != 0 {
		t.Fatalf("rejected request admitted work: active=%d upstream=%d", active, dispatched.Load())
	}
	done := make(chan struct{})
	go func() { s.workWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("rejected request retained admitted work")
	}
}

type rejectedReverseInput struct {
	io.Reader
	close func()
}

func (b *rejectedReverseInput) Close() error { b.close(); return nil }

func TestReverseMCPControlFailureDoesNotEnterWork(t *testing.T) {
	for _, readControl := range []bool{false, true} {
		t.Run(fmt.Sprintf("read-control-%v", readControl), func(t *testing.T) {
			s := &Server{}
			closed, interrupted, entered := false, false, false
			observe := func() {
				s.restart.mu.Lock()
				entered = entered || s.restart.active != 0
				s.restart.mu.Unlock()
			}
			req := httptest.NewRequest(http.MethodPost, "http://node/mcp", nil)
			req.Body = &rejectedReverseInput{Reader: strings.NewReader("x"), close: func() { closed = true; observe() }}
			if readControl {
				ctx := context.WithValue(req.Context(), reverseReadAbortKey{}, reverseReadAbort(func() error { interrupted = true; observe(); return nil }))
				req = req.WithContext(ctx)
			}
			response := httptest.NewRecorder()
			s.reverseMCPHandler().ServeHTTP(response, req)
			if response.Code != http.StatusServiceUnavailable || !closed || interrupted != readControl || entered || s.hubWaiters != nil {
				t.Fatalf("control refusal status=%d closed=%v interrupted=%v entered=%v upstream=%v", response.Code, closed, interrupted, entered, s.hubWaiters != nil)
			}
			assertReverseAdmissionIdle(t, s, &atomic.Int32{})
		})
	}
}
