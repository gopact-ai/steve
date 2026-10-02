package node

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/agentmcp"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/nodewire"
)

var _ reverseHTTPConnection = streamConn{}

func revokedReverseServer(t *testing.T) *agentmcp.Server {
	t.Helper()
	server, err := agentmcp.New(0, i18n.New(i18n.LocaleEN))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.PrepareExtras("original", "worker", "revoked", ""); err != nil {
		t.Fatal(err)
	}
	server.Revoke("revoked")
	done := make(chan error, 1)
	go func() { done <- server.Start(t.Context()) }()
	t.Cleanup(func() {
		server.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("collaboration server did not stop")
		}
	})
	return server
}

func reverseHTTPDialer(t *testing.T, address string) func(context.Context) (reverseHTTPConnection, error) {
	t.Helper()
	local, remote := net.Pipe()
	worker, hub := nodewire.NewMux(local, false), nodewire.NewMux(remote, true)
	connection := &conn{mux: hub}
	done := make(chan struct{})
	go func() {
		connection.serveReverse(func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", address)
		})
		close(done)
	}()
	t.Cleanup(func() {
		worker.Close()
		hub.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("reverse accept loop did not stop")
		}
	})
	return func(ctx context.Context) (reverseHTTPConnection, error) {
		stream, err := worker.Open(nodewire.OpenRequest{Kind: nodewire.StreamMCP})
		if err != nil {
			return nil, err
		}
		return streamConn{stream}, nil
	}
}

// A final rejection may close its stream before the request writer gets its
// next turn. Delay the response reader until that write error is handled: the
// received final response must not be replaced by the upload's EOF.
type earlyReplyOrder struct {
	reverseHTTPConnection
	closed chan struct{}
	once   sync.Once
	writes int
}

func (c *earlyReplyOrder) Read(p []byte) (int, error) {
	select {
	case <-c.closed:
		return c.reverseHTTPConnection.Read(p)
	case <-time.After(3 * time.Second):
		return 0, fmt.Errorf("response read was not released after upload ended")
	}
}

func (c *earlyReplyOrder) Write(p []byte) (int, error) {
	c.writes++
	if c.writes == 2 {
		select {
		case <-c.Done():
		case <-time.After(2 * time.Second):
			return 0, fmt.Errorf("upstream did not end its rejected request")
		}
	}
	return c.reverseHTTPConnection.Write(p)
}

func (c *earlyReplyOrder) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.reverseHTTPConnection.Close()
}

type reverseBody struct{ io.Reader }

func (*reverseBody) Close() error { return nil }

func TestReverseHTTPKeepsTheReceivedCredentialRefusal(t *testing.T) {
	server := revokedReverseServer(t)
	dial := reverseHTTPDialer(t, server.Addr())
	var calls atomic.Int32
	transport := newReverseHTTP(func(ctx context.Context) (reverseHTTPConnection, error) {
		calls.Add(1)
		connection, err := dial(ctx)
		if err != nil {
			return nil, err
		}
		return &earlyReplyOrder{reverseHTTPConnection: connection, closed: make(chan struct{})}, nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	payload := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"steve_context","arguments":{}}}`
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL(), &reverseBody{strings.NewReader(payload)})
	if err != nil {
		t.Fatal(err)
	}
	request.ContentLength = int64(len(payload))
	request.Header.Set("Authorization", "Bearer revoked")
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatalf("received credential refusal was lost to request-body failure: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusUnauthorized || !strings.Contains(string(body), `"unauthorized"`) {
		t.Fatalf("credential refusal = %d %s, %v", response.StatusCode, body, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("request opened %d streams, want exactly one", calls.Load())
	}
}

func TestReverseMCPCredentialRefusalDoesNotWaitForAnUpload(t *testing.T) {
	server := revokedReverseServer(t)
	worker := startNode(t, ServerConfig{Name: "worker", Token: "token", StateDir: t.TempDir()})
	registry := NewRegistry("hub", map[string]Config{"worker": {Addr: worker.Addr(), Token: "token"}})
	t.Cleanup(registry.Close)
	registry.SetMCPDialer(func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Addr())
	})
	endpoint, err := registry.MCPEndpoint(t.Context(), "worker")
	if err != nil {
		t.Fatal(err)
	}
	client, err := net.DialTimeout("tcp", strings.TrimSuffix(strings.TrimPrefix(endpoint, "http://"), "/mcp"), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	// Deliberately keep the connection open without supplying the body. A
	// rejected token must receive its real final response, not wait for it.
	_, err = io.WriteString(client, "POST /mcp HTTP/1.1\r\nHost: worker\r\nAuthorization: Bearer revoked\r\nContent-Type: application/json\r\nContent-Length: 1048576\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(client), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatalf("early refusal waited for the client upload: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusUnauthorized || !strings.Contains(string(body), `"unauthorized"`) {
		t.Fatalf("slow-upload refusal = %d %s, %v", response.StatusCode, body, err)
	}
}
