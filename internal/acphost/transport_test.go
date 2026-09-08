package acphost

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/permission"
)

// relayTransport is the seam's proof: it forks the agent locally but hands
// the host a TCP connection instead of the subprocess's own pipes, shuttling
// bytes between the two. That is exactly the shape a remote node has, so a
// host that drives an agent through this can drive one on another machine.
type relayTransport struct{ inner Transport }

func (t relayTransport) Name() string { return "relay/" + t.inner.Name() }

func (t relayTransport) Start(ctx context.Context) (Process, error) {
	proc, err := t.inner.Start(ctx)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		proc.Kill()
		return nil, err
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			close(accepted)
			return
		}
		accepted <- conn
	}()
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		proc.Kill()
		return nil, err
	}
	server, ok := <-accepted
	if !ok {
		client.Close()
		proc.Kill()
		return nil, io.ErrUnexpectedEOF
	}
	stdin := proc.Stdin()
	go func() {
		_, _ = io.Copy(stdin, server)
		// The host hanging up must reach the agent as EOF, or it never exits.
		_ = stdin.Close()
	}()
	go func() {
		_, _ = io.Copy(server, proc.Stdout())
		_ = server.Close()
	}()
	return &relayProcess{inner: proc, conn: client}, nil
}

type relayProcess struct {
	inner Process
	conn  net.Conn
}

func (p *relayProcess) Stdout() io.ReadCloser { return p.conn }
func (p *relayProcess) Stdin() io.WriteCloser { return p.conn }
func (p *relayProcess) Wait() error           { return p.inner.Wait() }
func (p *relayProcess) Kill()                 { _ = p.conn.Close(); p.inner.Kill() }
func (p *relayProcess) Stopped() bool         { return p.inner.Stopped() }

// TestTransportSeamCarriesACPOverAStream is the acceptance test for moving an
// agent off this machine: nothing about ACP depends on the pipes being a
// subprocess's, so the same round-trip must work over a socket.
func TestTransportSeamCarriesACPOverAStream(t *testing.T) {
	broker, err := permission.New("auto")
	if err != nil {
		t.Fatal(err)
	}
	h := New(Config{
		Transport: relayTransport{inner: LocalTransport{
			Command: buildMockAgent(t), ProcessDir: t.TempDir(),
		}},
		Permission: broker,
	})
	t.Cleanup(h.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sid, generation, err := h.OpenSession(ctx, "", SessionConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatalf("OpenSession over stream: %v", err)
	}
	out, _, err := h.Prompt(ctx, sid, generation, "hello world", nil)
	if err != nil {
		t.Fatalf("Prompt over stream: %v", err)
	}
	if out != "echo: hello world" {
		t.Fatalf("unexpected output over stream: %q", out)
	}
}

// TestConfigDefaultsToLocalTransport guards the shorthand: a Config that
// names a command and no transport still forks locally, which is what every
// existing caller relies on.
func TestConfigDefaultsToLocalTransport(t *testing.T) {
	h := New(Config{Command: "agent-bin", Args: []string{"acp"}, ProcessDir: "/tmp/x", Env: []string{"K=V"}})
	local, ok := h.cfg.Transport.(LocalTransport)
	if !ok {
		t.Fatalf("default transport = %T, want LocalTransport", h.cfg.Transport)
	}
	if local.Command != "agent-bin" || local.ProcessDir != "/tmp/x" ||
		len(local.Args) != 1 || local.Args[0] != "acp" ||
		len(local.Env) != 1 || local.Env[0] != "K=V" {
		t.Fatalf("config not carried into local transport: %+v", local)
	}
}
