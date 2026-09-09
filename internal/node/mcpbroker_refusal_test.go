package node

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func brokerOnSocket(t *testing.T) (*Broker, string, context.CancelFunc) {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "mcp.sock")
	broker := NewBroker(BrokerConfig{Socket: socket, MCPServers: map[string]MCPSpec{
		"echo": {Type: "stdio", Command: "sh", Args: []string{"-c", "cat"}},
	}})
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- broker.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-served })
	for i := 0; ; i++ {
		if _, err := os.Stat(socket); err == nil {
			return broker, socket, cancel
		}
		if i > 300 {
			t.Fatal("the broker never opened its socket")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A launcher whose binding is gone must read the refusal as an empty
// session, not as a connection error: the launcher sends the session's
// first bytes right behind the binding id, and a broker that closes
// with those bytes unread resets the connection instead of ending it.
func TestARefusedLaunchEndsTheConnectionInsteadOfResettingIt(t *testing.T) {
	broker, socket, _ := brokerOnSocket(t)
	binding, err := broker.Bind("echo", "a-gone", "codex")
	if err != nil {
		t.Fatal(err)
	}
	id := binding.Args[3]
	if n := broker.Release("a-gone"); n != 1 {
		t.Fatalf("released %d bindings", n)
	}
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// One write, so the broker's receive queue still holds the session's
	// first bytes when it lets a refused connection go.
	if _, err := conn.Write([]byte(id + "\n" + strings.Repeat("first bytes\n", 4096))); err != nil {
		t.Fatalf("send the launch: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	if _, err := io.Copy(&got, conn); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("a refused launch reported %v", err)
	}
	if got.Len() != 0 {
		t.Fatalf("a refused launch produced %q", got.String())
	}
}

// The launcher's own view of the same refusal: no output, no error.
func TestLaunchingAReleasedBindingEndsWithoutAnError(t *testing.T) {
	broker, socket, _ := brokerOnSocket(t)
	binding, err := broker.Bind("echo", "a-gone", "codex")
	if err != nil {
		t.Fatal(err)
	}
	id := binding.Args[3]
	broker.Release("a-gone")
	launch, stop := context.WithTimeout(t.Context(), 20*time.Second)
	defer stop()
	var out bytes.Buffer
	if err := LaunchBinding(launch, socket, id, strings.NewReader(strings.Repeat("payload\n", 100_000)), &out); err != nil {
		t.Fatalf("a released binding reported %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("a released binding produced %q", out.String())
	}
}
