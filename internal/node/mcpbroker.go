package node

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
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
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/ability"
)

// The MCP broker keeps the machine's MCP servers — and their credentials
// — with whoever runs it. An agent never receives a command line with
// secrets: it receives a launcher (steve-node, "mcp-launch", a binding
// id) that connects to the broker's socket, and the broker starts the
// real server with its env and pipes the two together. HTTP servers are
// reached through the broker's loopback proxy, which adds their headers.
//
// The broker is one type run two ways. Inside steve-node, with the
// servers from node.json, it is the simple deployment. As its own process
// — "steve-node mcp-broker -config mcp.json", ideally as its own user with
// mcp.json readable by it alone — it is the one where a process running
// as the node's user cannot read the secrets: the node then talks to it
// over the socket with a control token, exactly as the launcher does
// without one.
//
// Socket protocol, one line then a stream:
//
//	<binding id>                          launcher: pipe stdio to the server
//	BIND <token> <mcp> <attempt> <harness> → OK <json binding> | ERR <why>
//	RELEASE <token> <attempt>              → OK <n>
//	LIST <token>                           → OK <json {id: type}>

// BindingTTL bounds a binding's life when nobody releases it. A session
// that outlives it must be admitted again.
const BindingTTL = 24 * time.Hour

// LaunchVerb is the steve-node subcommand the launcher descriptor names.
const LaunchVerb = "mcp-launch"

var (
	// ErrNoSuchServer is a bind for a server the broker does not have.
	ErrNoSuchServer = errors.New("no such MCP server")
	// ErrUnbindable is a bind for a server the broker cannot hand out: an
	// unknown transport, or an HTTP server with no proxy listening.
	ErrUnbindable = errors.New("MCP server cannot be bound")
)

// BrokerConfig is what a broker process reads: the servers, the socket,
// the token the node must present for control commands, and where the
// launcher binary and working directory are.
type BrokerConfig struct {
	Socket        string             `json:"socket"`
	Token         string             `json:"token"`
	MCPServers    map[string]MCPSpec `json:"mcp_servers"`
	WorkspaceRoot string             `json:"workspace_root,omitempty"`
	// PortFile remembers the proxy port across restarts so descriptors
	// held by live sessions stay valid.
	PortFile string `json:"port_file,omitempty"`
	// Launcher is the steve-node binary launchers run; the broker's own
	// executable when empty.
	Launcher string `json:"launcher,omitempty"`
	// SocketMode is the socket's permission bits; 0600 when zero. A broker
	// serving another user's agents needs 0660 with a shared group.
	SocketMode os.FileMode `json:"socket_mode,omitempty"`
}

type mcpBinding struct {
	id, mcp, attempt, harness string
	expires                   time.Time
	running                   *exec.Cmd
}

// Broker holds the servers, mints bindings, launches, proxies.
type Broker struct {
	cfg BrokerConfig
	// work is installed only by an embedded node, sharing its restart gate.
	work func() (func(), error)

	mu        sync.Mutex
	bindings  map[string]mcpBinding
	proxyPort int
}

func NewBroker(cfg BrokerConfig) *Broker {
	return &Broker{cfg: cfg, bindings: map[string]mcpBinding{}}
}

// Serve listens on the socket and the loopback proxy until ctx ends.
func (b *Broker) Serve(ctx context.Context) error {
	if err := b.serveProxy(ctx); err != nil {
		log.Printf("steve-node: %v", err)
	}
	sock := b.cfg.Socket
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		return err
	}
	// A socket file left by an earlier broker is cleared; if it will not
	// go, Listen reports it.
	_ = os.Remove(sock)
	listener, err := net.Listen("unix", sock)
	if err != nil {
		return fmt.Errorf("mcp broker: listen %s: %w", sock, err)
	}
	mode := b.cfg.SocketMode
	if mode == 0 {
		mode = 0o600
	}
	if err := os.Chmod(sock, mode); err != nil {
		listener.Close()
		return err
	}
	go func() {
		<-ctx.Done()
		listener.Close()
	}()
	log.Printf("steve-node: mcp broker on %s: %d server(s)", sock, len(b.List()))
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("mcp broker: accept: %w", err)
		}
		go b.conn(ctx, conn)
	}
}

// SetServers replaces what the broker offers; bindings already made keep
// working against the new set by id.
func (b *Broker) SetServers(servers map[string]MCPSpec) {
	copied := make(map[string]MCPSpec, len(servers))
	for k, v := range servers {
		copied[k] = v
	}
	b.mu.Lock()
	b.cfg.MCPServers = copied
	b.mu.Unlock()
}

func (b *Broker) server(id string) (MCPSpec, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	spec, ok := b.cfg.MCPServers[id]
	return spec, ok
}

// List is what the broker offers: id → transport, nothing secret.
func (b *Broker) List() map[string]string {
	b.mu.Lock()
	servers := b.cfg.MCPServers
	b.mu.Unlock()
	out := make(map[string]string, len(servers))
	for id, spec := range servers {
		t := spec.Type
		if t == "" {
			t = "stdio"
		}
		out[id] = t
	}
	return out
}

// Bind mints a binding and describes how a session reaches it.
func (b *Broker) Bind(mcp, attempt, harness string) (ability.Binding, error) {
	spec, ok := b.server(mcp)
	if !ok {
		return ability.Binding{}, ErrNoSuchServer
	}
	switch spec.Type {
	case "", "stdio":
	case "http", "sse":
		if b.proxyAddr() == "" {
			return ability.Binding{}, fmt.Errorf("%w: the MCP proxy is not listening", ErrUnbindable)
		}
	default:
		return ability.Binding{}, fmt.Errorf("%w: unknown transport %q", ErrUnbindable, spec.Type)
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return ability.Binding{}, err
	}
	nb := mcpBinding{id: hex.EncodeToString(raw[:]), mcp: mcp, attempt: attempt, harness: harness, expires: time.Now().Add(BindingTTL)}
	b.mu.Lock()
	now := time.Now()
	for id, old := range b.bindings {
		if now.After(old.expires) {
			delete(b.bindings, id)
		}
	}
	b.bindings[nb.id] = nb
	b.mu.Unlock()
	if spec.Type == "http" || spec.Type == "sse" {
		return ability.Binding{Name: mcp, Transport: spec.Type, URL: b.proxyAddr() + "/b/" + nb.id}, nil
	}
	launcher := b.cfg.Launcher
	if launcher == "" {
		exe, err := os.Executable()
		if err != nil {
			return ability.Binding{}, err
		}
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		launcher = exe
	}
	return ability.Binding{Name: mcp, Transport: "stdio", Command: launcher, Args: []string{LaunchVerb, "-socket", b.cfg.Socket, nb.id}}, nil
}

// Release drops an attempt's bindings and stops the servers behind them.
func (b *Broker) Release(attempt string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for id, nb := range b.bindings {
		if nb.attempt != attempt {
			continue
		}
		if nb.running != nil {
			if err := killProcessGroup(nb.running); err != nil {
				log.Printf("steve-node: mcp %s for attempt %s: kill: %v", nb.mcp, attempt, err)
			}
		}
		delete(b.bindings, id)
		n++
	}
	return n
}

func (b *Broker) binding(id string) (mcpBinding, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	nb, ok := b.bindings[id]
	if !ok || time.Now().After(nb.expires) {
		return mcpBinding{}, false
	}
	return nb, true
}

func (b *Broker) attach(id string, cmd *exec.Cmd) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if nb, ok := b.bindings[id]; ok {
		nb.running = cmd
		b.bindings[id] = nb
	}
}

func (b *Broker) control(token string) bool {
	return b.cfg.Token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(b.cfg.Token)) == 1
}

// conn serves one connection: a control command, or a launcher.
func (b *Broker) conn(ctx context.Context, c net.Conn) {
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	if err := c.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return
	}
	// The reader stays: bytes after the first line are the session's
	// first bytes, and they may already sit in its buffer.
	reader := bufio.NewReaderSize(c, 4096)
	line, err := reader.ReadString('\n')
	if err != nil {
		return
	}
	// Clearing a deadline on a live socket cannot fail in a way the
	// session's reads would not report.
	_ = c.SetReadDeadline(time.Time{})
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return
	}
	reply := func(s string) { _, _ = io.WriteString(c, s+"\n") }
	if fields[0] != "LIST" && b.work != nil {
		done, err := b.work()
		if err != nil {
			reply("ERR node is restarting")
			return
		}
		defer done()
	}
	switch fields[0] {
	case "BIND":
		if len(fields) != 5 || !b.control(fields[1]) {
			reply("ERR refused")
			return
		}
		binding, err := b.Bind(fields[2], fields[3], fields[4])
		if err != nil {
			reply("ERR " + err.Error())
			return
		}
		raw, _ := json.Marshal(binding)
		reply("OK " + string(raw))
	case "RELEASE":
		if len(fields) != 3 || !b.control(fields[1]) {
			reply("ERR refused")
			return
		}
		reply("OK " + strconv.Itoa(b.Release(fields[2])))
	case "LIST":
		if len(fields) != 2 || !b.control(fields[1]) {
			reply("ERR refused")
			return
		}
		raw, _ := json.Marshal(b.List())
		reply("OK " + string(raw))
	default:
		if len(fields) != 1 {
			return
		}
		b.launch(ctx, c, reader, fields[0])
	}
}

// launch starts the server behind a binding and pipes it to the launcher.
func (b *Broker) launch(ctx context.Context, c net.Conn, reader io.Reader, id string) {
	nb, ok := b.binding(id)
	if !ok {
		log.Printf("steve-node: mcp broker: unknown or expired binding")
		return
	}
	spec, ok := b.server(nb.mcp)
	if !ok || (spec.Type != "stdio" && spec.Type != "") {
		log.Printf("steve-node: mcp broker: %s is not a stdio server here", nb.mcp)
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
	if b.cfg.WorkspaceRoot != "" {
		cmd.Dir = b.cfg.WorkspaceRoot
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	cmd.Stderr = prefixedLog{prefix: "steve-node: mcp " + nb.mcp + ": "}
	if err := cmd.Start(); err != nil {
		log.Printf("steve-node: mcp broker: start %s: %v", nb.mcp, err)
		return
	}
	b.attach(nb.id, cmd)
	log.Printf("steve-node: mcp %s started for attempt %s (%s)", nb.mcp, nb.attempt, nb.harness)
	// The session lasts as long as either pump: when one side ends the
	// server is torn down whole, and its exit status is not the
	// session's to report.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(stdin, reader)
		_ = stdin.Close()
	}()
	_, _ = io.Copy(c, stdout)
	_ = killProcessGroup(cmd)
	_ = cmd.Wait()
	<-done
	log.Printf("steve-node: mcp %s for attempt %s ended", nb.mcp, nb.attempt)
}

// serveProxy listens on the loopback for http/sse servers: a binding's
// URL is /b/<id>/…, and the proxy adds the server's headers on the way
// out. The port is remembered so a restart keeps old descriptors valid.
func (b *Broker) serveProxy(ctx context.Context) error {
	var listener net.Listener
	var err error
	if preferred := rememberedPortIn(b.cfg.PortFile); preferred > 0 {
		listener, err = net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(preferred))
	}
	if listener == nil {
		listener, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		return fmt.Errorf("mcp proxy: listen: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	b.mu.Lock()
	b.proxyPort = port
	b.mu.Unlock()
	if b.cfg.PortFile != "" {
		if err := os.WriteFile(b.cfg.PortFile, []byte(strconv.Itoa(port)), 0o600); err != nil {
			log.Printf("steve-node: mcp proxy: remember port: %v", err)
		}
	}
	server := &http.Server{Handler: http.HandlerFunc(b.proxy), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		// Shutdown; Serve reports the close as ErrServerClosed.
		_ = server.Close()
	}()
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("steve-node: mcp proxy: %v", err)
		}
	}()
	return nil
}

func rememberedPortIn(path string) int {
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

func (b *Broker) proxyAddr() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.proxyPort == 0 {
		return ""
	}
	return "http://127.0.0.1:" + strconv.Itoa(b.proxyPort)
}

func (b *Broker) proxy(w http.ResponseWriter, r *http.Request) {
	if b.work != nil {
		done, err := b.work()
		if err != nil {
			http.Error(w, "node is restarting", http.StatusServiceUnavailable)
			return
		}
		defer done()
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/b/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	id, tail, _ := strings.Cut(rest, "/")
	nb, ok := b.binding(id)
	if !ok {
		http.Error(w, "unknown or expired binding", http.StatusForbidden)
		return
	}
	spec, ok := b.server(nb.mcp)
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

// mcpBroker is how the node reaches its broker: in-process, or over the
// socket with the control token.
