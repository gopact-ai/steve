package node

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/ability"
	"github.com/gopact-ai/steve/internal/mcpprobe"
	"github.com/gopact-ai/steve/internal/nodewire"
)

// probeMu lets one probe run at a time on a machine: a probe starts a
// server, and two at once would compete for the same credentials and
// ports.
var probeMu sync.Mutex

// mcpProbe serves StreamMCPProbe: it binds the named server the way a
// session would — through the broker, so credentials and headers are
// the broker's business — asks it for its tools, and releases it.
func (s *Server) mcpProbe(ctx context.Context, stream *nodewire.Stream) {
	defer stream.Close()
	name := strings.TrimSpace(stream.Request().Command)
	reply := func(r nodewire.MCPProbeReply) {
		if err := json.NewEncoder(stream).Encode(r); err != nil {
			slog.Error(fmt.Sprintf("steve-node: mcp probe reply: %v", err))
		}
	}
	if name == "" {
		reply(nodewire.MCPProbeReply{Error: "a server name is required"})
		return
	}
	if _, ok := s.conf().MCPServers[name]; !ok {
		reply(nodewire.MCPProbeReply{Error: fmt.Sprintf("no MCP server %q on this machine", name)})
		return
	}
	probeMu.Lock()
	defer probeMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	attempt := "probe:" + fmt.Sprint(time.Now().UnixNano())
	binding, err := s.broker.Bind(ctx, name, attempt, "probe")
	if err != nil {
		reply(nodewire.MCPProbeReply{Error: "bind: " + err.Error()})
		return
	}
	defer func() {
		if _, err := s.broker.Release(context.Background(), attempt); err != nil {
			slog.Error(fmt.Sprintf("steve-node: mcp probe: release %s: %v", name, err), "mcp", name)
		}
	}()
	result, err := mcpprobe.Probe(ctx, mcpprobe.Server{Type: binding.Transport, Command: binding.Command, Args: binding.Args, URL: binding.URL})
	if err != nil {
		reply(nodewire.MCPProbeReply{Error: err.Error()})
		return
	}
	out := nodewire.MCPProbeReply{ServerName: result.ServerName, ServerVersion: result.ServerVersion, Protocol: result.Protocol, Digest: result.Digest, ElapsedMS: result.Elapsed.Milliseconds(), Tools: []nodewire.MCPTool{}}
	for _, t := range result.Tools {
		out.Tools = append(out.Tools, nodewire.MCPTool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
	}
	reply(out)
}
func portFile(cfg ServerConfig) string {
	if cfg.StateDir == "" {
		return ""
	}
	return filepath.Join(cfg.StateDir, "mcp.port")
}

func rememberedPort(cfg ServerConfig) int {
	path := portFile(cfg)
	if path == "" {
		return 0
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	port, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || port <= 0 || port > 65535 {
		return 0
	}
	return port
}

func (s *Server) rememberPort(port int) {
	path := portFile(s.conf())
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(port)+"\n"), 0o600); err != nil {
		slog.Error(fmt.Sprintf("steve-node: remember MCP port: %v", err))
	}
}

type mcpBroker interface {
	List(ctx context.Context) (map[string]string, error)
	Bind(ctx context.Context, mcp, attempt, harness string) (ability.Binding, error)
	Release(ctx context.Context, attempt string) (int, error)
}

type localBroker struct{ b *Broker }

func (l localBroker) List(context.Context) (map[string]string, error) { return l.b.List(), nil }
func (l localBroker) Bind(_ context.Context, mcp, attempt, harness string) (ability.Binding, error) {
	return l.b.Bind(mcp, attempt, harness)
}
func (l localBroker) Release(_ context.Context, attempt string) (int, error) {
	return l.b.Release(attempt), nil
}

// remoteBroker speaks the socket protocol to a broker in another process.
type remoteBroker struct {
	socket, token string
}

func (r remoteBroker) call(ctx context.Context, line string) (string, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", r.socket)
	if err != nil {
		return "", fmt.Errorf("mcp broker at %s: %w", r.socket, err)
	}
	defer c.Close()
	if err := c.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		return "", err
	}
	if _, err := io.WriteString(c, line+"\n"); err != nil {
		return "", err
	}
	reply, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("mcp broker: %w", err)
	}
	reply = strings.TrimSpace(reply)
	if rest, ok := strings.CutPrefix(reply, "OK "); ok {
		return rest, nil
	}
	if reply == "OK" {
		return "", nil
	}
	why := strings.TrimPrefix(reply, "ERR ")
	switch {
	case strings.Contains(why, ErrNoSuchServer.Error()):
		return "", ErrNoSuchServer
	case strings.Contains(why, ErrUnbindable.Error()):
		return "", fmt.Errorf("%w: %s", ErrUnbindable, why)
	}
	return "", errors.New("mcp broker: " + why)
}

func (r remoteBroker) List(ctx context.Context) (map[string]string, error) {
	raw, err := r.call(ctx, "LIST "+r.token)
	if err != nil {
		return nil, err
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (r remoteBroker) Bind(ctx context.Context, mcp, attempt, harness string) (ability.Binding, error) {
	for _, f := range []string{mcp, attempt, harness} {
		if strings.ContainsAny(f, " \t\n") || f == "" {
			return ability.Binding{}, fmt.Errorf("mcp broker: bad field %q", f)
		}
	}
	raw, err := r.call(ctx, "BIND "+r.token+" "+mcp+" "+attempt+" "+harness)
	if err != nil {
		return ability.Binding{}, err
	}
	var out ability.Binding
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return ability.Binding{}, err
	}
	return out, nil
}

func (r remoteBroker) Release(ctx context.Context, attempt string) (int, error) {
	raw, err := r.call(ctx, "RELEASE "+r.token+" "+attempt)
	if err != nil {
		return 0, err
	}
	n, _ := strconv.Atoi(strings.TrimSpace(raw))
	return n, nil
}

// releaseAttempt serves StreamRelease.
func (s *Server) releaseAttempt(stream *nodewire.Stream) {
	// Process release is explicit; an attempt only releases its own MCP
	// bindings because other ACP sessions may share the harness process.
	if stream.Request().Stream != "" {
		s.releaseProcess(stream)
		return
	}
	defer stream.Close()
	attempt := strings.TrimSpace(stream.Request().Command)
	if attempt == "" || s.broker == nil {
		closeStream(stream, nodewire.ExitPrefix+"2")
		return
	}
	n, err := s.broker.Release(context.Background(), attempt)
	if err != nil {
		slog.Error(fmt.Sprintf("steve-node: release %s: %v", attempt, err), "attempt", attempt)
		closeStream(stream, nodewire.ExitPrefix+"1")
		return
	}
	if n > 0 {
		slog.Info(fmt.Sprintf("steve-node: released %d MCP binding(s) of attempt %s", n, attempt), "attempt", attempt)
	}
	closeStream(stream, nodewire.ExitPrefix+"0")
}

// LaunchBinding is the launcher side: it connects to the broker, names
// its binding, and pipes stdin/stdout to the MCP server the broker starts.
// It is what "steve-node mcp-launch" runs, and it carries no secret.
func LaunchBinding(ctx context.Context, socket, id string, stdin io.Reader, stdout io.Writer) error {
	if socket == "" || id == "" {
		return errors.New("mcp-launch: socket and binding id are required")
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		return fmt.Errorf("mcp-launch: connect broker: %w", err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, id+"\n"); err != nil {
		return err
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		// The input pump ends with the agent's stdin; the output copy
		// below is what reports the session's end.
		_, _ = io.Copy(conn, stdin)
		if cw, ok := conn.(halfCloser); ok {
			// Half-closing tells the server the agent's input ended; a
			// failure here means the socket is already gone, and the copy
			// of its output below reports that.
			_ = cw.CloseWrite()
		}
	}()
	_, err = io.Copy(stdout, conn)
	select {
	case <-done:
	case <-ctx.Done():
	}
	return err
}

// halfCloser closes the sending side of a connection so the server sees
// the agent's EOF while its own answer is still flowing back. The
// broker's unix socket has one.
type halfCloser interface {
	CloseWrite() error
}

var _ halfCloser = (*net.UnixConn)(nil)

// SocketPath is where the node's own in-process broker listens.
func (s *Server) SocketPath() string { return filepath.Join(s.conf().StateDir, "mcp.sock") }
