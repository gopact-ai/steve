package node

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gopact-ai/steve/internal/ability"
	"github.com/gopact-ai/steve/internal/nodewire"
)

// The MCP broker keeps the machine's MCP servers — and their credentials
// — on the machine. An agent never receives a command line with secrets:
// it receives a launcher (this binary, "mcp-launch", a binding id) that
// connects to the broker's socket, and the broker starts the real server
// with its env and pipes the two together. A binding is minted at
// admission for one attempt's session and dies with the node process or
// its TTL.
//
// Privilege separation is a deployment matter this code cannot enforce:
// the broker runs as the node's user, and a process of the same user can
// read the node's config. Running the node under its own user, with the
// harness homes and workspaces shared but the config not, is the intended
// shape.

// BindingTTL bounds a binding's life. A session that outlives it must be
// admitted again.
const BindingTTL = 24 * time.Hour

// LaunchVerb is the steve-node subcommand the launcher descriptor names.
const LaunchVerb = "mcp-launch"

type mcpBinding struct {
	id, mcp, attempt, harness string
	expires                   time.Time
	// running is the server started for this binding, if one is up.
	running *exec.Cmd
}

// attach remembers the server started for a binding so a release can
// stop it.
func (s *Server) attach(id string, cmd *exec.Cmd) {
	s.bindMu.Lock()
	defer s.bindMu.Unlock()
	if b, ok := s.bindings[id]; ok {
		b.running = cmd
		s.bindings[id] = b
	}
}

// releaseBindings drops every binding of an attempt and stops the
// servers behind them. It is what the hub asks for when the attempt ends,
// and what makes a binding's life the attempt's rather than a TTL's.
func (s *Server) releaseBindings(attempt string) int {
	s.bindMu.Lock()
	defer s.bindMu.Unlock()
	n := 0
	for id, b := range s.bindings {
		if b.attempt != attempt {
			continue
		}
		if b.running != nil {
			_ = killProcessGroup(b.running)
		}
		delete(s.bindings, id)
		n++
	}
	return n
}

// releaseAttempt serves StreamRelease.
func (s *Server) releaseAttempt(stream *nodewire.Stream) {
	defer stream.Close()
	attempt := strings.TrimSpace(stream.Request().Command)
	if attempt == "" {
		_ = stream.CloseWithReason(nodewire.ExitPrefix + "2")
		return
	}
	if n := s.releaseBindings(attempt); n > 0 {
		log.Printf("steve-node: released %d MCP binding(s) of attempt %s", n, attempt)
	}
	_ = stream.CloseWithReason(nodewire.ExitPrefix + "0")
}

// serveProxy listens on the loopback for http/sse MCP servers: a binding's
// URL is /b/<id>/…, and the proxy adds the server's configured headers on
// the way out. The port is remembered so a restart keeps old descriptors
// valid for the sessions that hold them.
func (s *Server) serveProxy(ctx context.Context) error {
	preferred := rememberedProxyPort(s.cfg)
	var listener net.Listener
	var err error
	if preferred > 0 {
		listener, err = net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(preferred))
	}
	if listener == nil {
		listener, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		return fmt.Errorf("mcp proxy: listen: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	s.bindMu.Lock()
	s.proxyPort = port
	s.bindMu.Unlock()
	if path := proxyPortFile(s.cfg); path != "" {
		_ = os.WriteFile(path, []byte(strconv.Itoa(port)), 0o600)
	}
	server := &http.Server{Handler: http.HandlerFunc(s.proxy), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("steve-node: mcp proxy: %v", err)
		}
	}()
	return nil
}

func proxyPortFile(cfg ServerConfig) string {
	if cfg.StateDir == "" {
		return ""
	}
	return filepath.Join(cfg.StateDir, "mcp-proxy.port")
}

func rememberedProxyPort(cfg ServerConfig) int {
	raw, err := os.ReadFile(proxyPortFile(cfg))
	if err != nil {
		return 0
	}
	port, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || port <= 0 || port > 65535 {
		return 0
	}
	return port
}

func (s *Server) proxyAddr() string {
	s.bindMu.Lock()
	defer s.bindMu.Unlock()
	if s.proxyPort == 0 {
		return ""
	}
	return "http://127.0.0.1:" + strconv.Itoa(s.proxyPort)
}

// proxy forwards one request for a binding to the real server.
func (s *Server) proxy(w http.ResponseWriter, r *http.Request) {
	rest, ok := strings.CutPrefix(r.URL.Path, "/b/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	id, tail, _ := strings.Cut(rest, "/")
	b, ok := s.binding(id)
	if !ok {
		http.Error(w, "unknown or expired binding", http.StatusForbidden)
		return
	}
	spec, ok := s.cfg.MCPServers[b.mcp]
	if !ok || (spec.Type != "http" && spec.Type != "sse") {
		http.Error(w, "not an http server", http.StatusBadGateway)
		return
	}
	target, err := url.Parse(spec.URL)
	if err != nil {
		http.Error(w, "bad upstream url", http.StatusBadGateway)
		return
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.URL.Path = joinPath(target.Path, tail)
			pr.Out.URL.RawPath = ""
			pr.Out.Host = target.Host
			for k, v := range spec.Headers {
				pr.Out.Header.Set(k, v)
			}
		},
		FlushInterval: -1,
	}
	rp.ServeHTTP(w, r)
}

func joinPath(base, tail string) string {
	base = strings.TrimSuffix(base, "/")
	if tail == "" {
		if base == "" {
			return "/"
		}
		return base
	}
	return base + "/" + tail
}

// SocketPath is the broker's Unix socket.
func (s *Server) SocketPath() string { return filepath.Join(s.cfg.StateDir, "mcp.sock") }

// serveBroker listens on the socket until ctx ends.
func (s *Server) serveBroker(ctx context.Context) error {
	sock := s.SocketPath()
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		return err
	}
	_ = os.Remove(sock)
	listener, err := net.Listen("unix", sock)
	if err != nil {
		return fmt.Errorf("mcp broker: listen %s: %w", sock, err)
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		listener.Close()
		return err
	}
	go func() {
		<-ctx.Done()
		listener.Close()
	}()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("mcp broker: accept: %w", err)
		}
		go s.brokerConn(ctx, conn)
	}
}

// bind mints a binding for one MCP server, for one attempt.
func (s *Server) bind(mcp, attempt, harness string) (mcpBinding, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return mcpBinding{}, err
	}
	b := mcpBinding{id: hex.EncodeToString(raw[:]), mcp: mcp, attempt: attempt, harness: harness, expires: time.Now().Add(BindingTTL)}
	s.bindMu.Lock()
	defer s.bindMu.Unlock()
	if s.bindings == nil {
		s.bindings = map[string]mcpBinding{}
	}
	now := time.Now()
	for id, old := range s.bindings {
		if now.After(old.expires) {
			delete(s.bindings, id)
		}
	}
	s.bindings[b.id] = b
	return b, nil
}

func (s *Server) binding(id string) (mcpBinding, bool) {
	s.bindMu.Lock()
	defer s.bindMu.Unlock()
	b, ok := s.bindings[id]
	if !ok || time.Now().After(b.expires) {
		return mcpBinding{}, false
	}
	return b, true
}

// descriptor is what the hub writes into session/new for a binding: this
// binary, the launch verb, the socket and the id. No secret rides along.
func (s *Server) descriptor(b mcpBinding) (ability.Binding, error) {
	exe, err := os.Executable()
	if err != nil {
		return ability.Binding{}, err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	spec := s.cfg.MCPServers[b.mcp]
	if spec.Type == "http" || spec.Type == "sse" {
		addr := s.proxyAddr()
		if addr == "" {
			return ability.Binding{}, errors.New("the MCP proxy is not listening")
		}
		return ability.Binding{Name: b.mcp, Transport: spec.Type, URL: addr + "/b/" + b.id}, nil
	}
	return ability.Binding{Name: b.mcp, Transport: "stdio", Command: exe, Args: []string{LaunchVerb, "-socket", s.SocketPath(), b.id}}, nil
}

// brokerConn serves one launcher: reads the binding id, starts the MCP
// server with the machine's env for it, and pipes the two until either
// side closes. The server dies with the connection.
func (s *Server) brokerConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	// The reader stays: bytes after the id line are the session's first
	// bytes, and they may already sit in its buffer.
	reader := bufio.NewReaderSize(conn, 4096)
	line, err := reader.ReadString('\n')
	if err != nil {
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	id := strings.TrimSpace(line)
	b, ok := s.binding(id)
	if !ok {
		log.Printf("steve-node: mcp broker: unknown or expired binding")
		return
	}
	spec, ok := s.cfg.MCPServers[b.mcp]
	if !ok || (spec.Type != "stdio" && spec.Type != "") {
		log.Printf("steve-node: mcp broker: %s is not a stdio server here", b.mcp)
		return
	}
	cmd := exec.CommandContext(ctx, spec.Command, spec.Args...)
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = os.Environ()
	for k, v := range spec.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if s.cfg.WorkspaceRoot != "" {
		cmd.Dir = s.cfg.WorkspaceRoot
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	cmd.Stderr = prefixedLog{prefix: "steve-node: mcp " + b.mcp + ": "}
	if err := cmd.Start(); err != nil {
		log.Printf("steve-node: mcp broker: start %s: %v", b.mcp, err)
		return
	}
	s.attach(b.id, cmd)
	log.Printf("steve-node: mcp %s started for attempt %s (%s)", b.mcp, b.attempt, b.harness)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(stdin, reader)
		_ = stdin.Close()
	}()
	_, _ = io.Copy(conn, stdout)
	_ = killProcessGroup(cmd)
	_ = cmd.Wait()
	<-done
	log.Printf("steve-node: mcp %s for attempt %s ended", b.mcp, b.attempt)
}

// prefixedLog writes a child's stderr to the node log, line by line.
type prefixedLog struct{ prefix string }

func (p prefixedLog) Write(b []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if strings.TrimSpace(line) != "" {
			log.Printf("%s%s", p.prefix, line)
		}
	}
	return len(b), nil
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
		_, _ = io.Copy(conn, stdin)
		if cw, ok := conn.(interface{ CloseWrite() error }); ok {
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
