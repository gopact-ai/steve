package node

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/acphost"
	"github.com/gopact-ai/steve/internal/nodewire"
)

// HarnessSpec is one agent runtime this node can start. The command line is
// a node-local fact — which binaries exist and which model endpoints are
// reachable is a property of this machine, not of the hub's config file.
type HarnessSpec struct {
	Command    string   `json:"command"`
	Args       []string `json:"args,omitempty"`
	Env        []string `json:"env,omitempty"`
	ProcessDir string   `json:"process_dir,omitempty"`
	// Models this harness offers here. The running agent remains the
	// authority on what it will actually accept; this is what the node
	// promises so the hub can refuse an impossible placement up front.
	Models []string `json:"models,omitempty"`
	// Slots caps concurrent sessions of this harness on this node. The hub
	// leases one slot per attempt; zero is unlimited.
	Slots int `json:"slots,omitempty"`
}

// ServerConfig is the node's own configuration file.
type ServerConfig struct {
	Name          string                 `json:"name"`
	Listen        string                 `json:"listen"`
	Token         string                 `json:"token"`
	Harnesses     map[string]HarnessSpec `json:"harnesses"`
	Capabilities  []string               `json:"capabilities,omitempty"`
	WorkspaceRoot string                 `json:"workspace_root,omitempty"`
	StateDir      string                 `json:"state_dir,omitempty"`
}

// Server accepts hub connections and runs agents on this machine.
type Server struct {
	cfg ServerConfig

	grantsMu sync.Mutex
	grants   map[string]peerGrant

	mu       sync.Mutex
	mcpPort  int
	listener net.Listener
}

func NewServer(cfg ServerConfig) *Server { return &Server{cfg: cfg, mcpPort: rememberedPort(cfg)} }

// Serve blocks until ctx ends or the listener fails.
func (s *Server) Serve(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.cfg.Listen, err)
	}
	s.mu.Lock()
	s.listener = listener
	s.mu.Unlock()
	log.Printf("steve-node: %s listening on %s", s.cfg.Name, listener.Addr())
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	for {
		socket, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}
		go s.handle(ctx, socket)
	}
}

// Addr is the bound address, useful once Listen was ":0".
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return s.cfg.Listen
	}
	return s.listener.Addr().String()
}

func (s *Server) handle(ctx context.Context, socket net.Conn) {
	defer socket.Close()
	// The reverse listener is bound before the advert so its port can be
	// reported in the same breath: the hub bakes that URL into the session
	// fingerprint, so it has to be known before any session opens.
	mcp, err := s.listenMCP()
	if err != nil {
		log.Printf("steve-node: reverse MCP listener: %v — agents here lose the send primitive", err)
	}
	advert := s.advert()
	if mcp != nil {
		advert.MCPPort = mcp.Addr().(*net.TCPAddr).Port
		defer mcp.Close()
	}
	hello, err := nodewire.AcceptWith(socket, s.validToken, advert)
	if err != nil {
		log.Printf("steve-node: handshake from %s: %v", socket.RemoteAddr(), err)
		return
	}
	if name, ok := s.grantedName(hello.Token); ok {
		// A peer, not the hub: it may take the one blob it was granted and
		// nothing else, and the grant is spent by the connection.
		s.servePeer(ctx, socket, hello, name)
		return
	}
	log.Printf("steve-node: hub %q connected from %s", hello.Hub, socket.RemoteAddr())

	mux := nodewire.NewMux(socket, false)
	defer mux.Close()
	if mcp != nil {
		go s.forwardMCP(mux, mcp)
	}
	for {
		stream, err := mux.Accept(ctx)
		if err != nil {
			log.Printf("steve-node: hub %q disconnected", hello.Hub)
			return
		}
		switch stream.Request().Kind {
		case nodewire.StreamExec:
			go s.runCommand(ctx, stream)
		case nodewire.StreamAdvert:
			go s.sendAdvert(stream)
		case nodewire.StreamBlob:
			go s.transferBlob(ctx, stream)
		case nodewire.StreamGrant:
			go s.grant(stream)
		case nodewire.StreamFetch:
			go s.fetch(ctx, stream)
		default:
			go s.runAgent(ctx, stream)
		}
	}
}

// commandTimeout bounds one verification command. A check that has not
// finished in this long is not a check anyone is waiting on.
const commandTimeout = 10 * time.Minute

// runCommand executes a verification command in the requested directory and
// reports its exit status as the stream's close reason. The command comes
// from a plan the hub validated, and runs as this node's user — the same
// authority the agent it verifies already has here.
func (s *Server) runCommand(ctx context.Context, stream *nodewire.Stream) {
	req := stream.Request()
	dir := req.Dir
	if dir == "" {
		dir = s.cfg.WorkspaceRoot
	}
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", req.Command)
	cmd.Dir = dir
	cmd.Stdout = stream
	cmd.Stderr = stream
	err := cmd.Run()
	code := 0
	if err != nil {
		code = -1
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		}
	}
	log.Printf("steve-node: verify %q in %s -> exit %d", req.Command, dir, code)
	_ = stream.CloseWithReason(fmt.Sprintf("%s%d", nodewire.ExitPrefix, code))
}

// runAgent starts the requested harness locally and shuttles its stdio over
// the stream. Everything ACP needs is in those bytes.
func (s *Server) runAgent(ctx context.Context, stream *nodewire.Stream) {
	defer stream.Close()
	req := stream.Request()
	if req.Kind != nodewire.StreamACP {
		return
	}
	spec, ok := s.cfg.Harnesses[req.Harness]
	if !ok {
		log.Printf("steve-node: hub asked for unknown harness %q", req.Harness)
		return
	}
	proc, err := acphost.LocalTransport{
		Command: spec.Command, Args: spec.Args,
		ProcessDir: s.processDir(spec), Env: spec.Env,
	}.Start(ctx)
	if err != nil {
		log.Printf("steve-node: start %s: %v", req.Harness, err)
		return
	}
	log.Printf("steve-node: session started on %s", req.Harness)

	var once sync.Once
	stop := func() { once.Do(func() { proc.Kill() }) }
	defer func() {
		stop()
		_ = proc.Wait()
		log.Printf("steve-node: session on %s ended", req.Harness)
	}()

	toAgent := make(chan struct{})
	go func() {
		defer close(toAgent)
		_, _ = io.Copy(proc.Stdin(), stream)
		// The hub hanging up must reach the agent as EOF, or it lingers.
		_ = proc.Stdin().Close()
	}()
	_, _ = io.Copy(stream, proc.Stdout())
	stop()
	<-toAgent
}

func (s *Server) processDir(spec HarnessSpec) string {
	if spec.ProcessDir != "" {
		return spec.ProcessDir
	}
	return s.cfg.WorkspaceRoot
}

// advert reports what this machine can honestly do. Harnesses whose command
// is not on this node's PATH are listed as missing rather than omitted: a
// roster that hides what is broken sends the hub hunting for a node that
// silently vanished.
func (s *Server) advert() nodewire.Advert {
	adv := Advertise(s.cfg.Name, s.cfg.Harnesses, s.cfg.Capabilities)
	adv.WorkspaceRoot = s.cfg.WorkspaceRoot
	adv.StateDir = s.cfg.StateDir
	return adv
}

// Advertise describes the machine this process runs on: its identity, and
// each configured harness checked against the PATH right here. The hub
// uses it for its own machine, so the fleet has one shape for every node
// and the coordinating one is not described by its config alone.
func Advertise(name string, specs map[string]HarnessSpec, caps []string) nodewire.Advert {
	ids := make([]string, 0, len(specs))
	for id := range specs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	harnesses := make([]nodewire.Harness, 0, len(ids))
	for _, id := range ids {
		spec := specs[id]
		h := nodewire.Harness{ID: id, Command: spec.Command, Models: spec.Models, Slots: spec.Slots}
		if _, err := exec.LookPath(spec.Command); err != nil {
			h.Missing = fmt.Sprintf("%q not on this node's PATH", spec.Command)
		}
		harnesses = append(harnesses, h)
	}
	hostname, ips := nodewire.Identity()
	return nodewire.Advert{
		Node: name, OS: runtime.GOOS, Arch: runtime.GOARCH,
		BuildVersion: nodewire.Version(), Hostname: hostname, IPs: ips,
		Harnesses: harnesses, Capabilities: caps,
		Git: gitVersion(),
	}
}

// sendAdvert answers a hub asking "check yourself again": the same advert
// the handshake carried, computed now, so a harness installed since then
// is seen without dropping the connection.
func (s *Server) sendAdvert(stream *nodewire.Stream) {
	defer stream.Close()
	if err := json.NewEncoder(stream).Encode(s.advert()); err != nil {
		log.Printf("steve-node: send advert: %v", err)
	}
}

// listenMCP binds the loopback port a local agent will call. The port is
// remembered across restarts for the same reason the hub remembers its own:
// it lives inside every session's capability fingerprint, and a new port
// would ask every live conversation for /new after a node restart.
func (s *Server) listenMCP() (net.Listener, error) {
	s.mu.Lock()
	preferred := s.mcpPort
	s.mu.Unlock()
	if preferred > 0 {
		if listener, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(preferred)); err == nil {
			return listener, nil
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	s.mu.Lock()
	s.mcpPort = port
	s.mu.Unlock()
	s.rememberPort(port)
	return listener, nil
}

// forwardMCP tunnels each local MCP connection to the hub over the same
// multiplexed link the sessions use.
func (s *Server) forwardMCP(mux *nodewire.Mux, listener net.Listener) {
	for {
		local, err := listener.Accept()
		if err != nil {
			return
		}
		go func(local net.Conn) {
			defer local.Close()
			stream, err := mux.Open(nodewire.OpenRequest{Kind: nodewire.StreamMCP})
			if err != nil {
				return
			}
			defer stream.Close()
			done := make(chan struct{})
			go func() {
				_, _ = io.Copy(stream, local)
				close(done)
			}()
			_, _ = io.Copy(local, stream)
			<-done
		}(local)
	}
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
	path := portFile(s.cfg)
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(port)+"\n"), 0o600); err != nil {
		log.Printf("steve-node: remember MCP port: %v", err)
	}
}

// gitVersion reports the node's git, or nothing.
func gitVersion() string {
	out, err := exec.Command("git", "--version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(string(out), "git version "))
}

// BlobDir is where the hub's files land on this node.
func (s *Server) BlobDir() string { return filepath.Join(s.cfg.StateDir, "blobs") }

// transferBlob serves one "put <name>" or "get <name>". Names are single
// path segments; the hub cannot reach outside the blob directory.
func (s *Server) transferBlob(ctx context.Context, stream *nodewire.Stream) {
	req := stream.Request()
	verb, name, _ := strings.Cut(req.Command, " ")
	name = strings.TrimSpace(name)
	if name == "" || name != filepath.Base(name) || strings.HasPrefix(name, ".") {
		_ = stream.CloseWithReason(nodewire.ExitPrefix + "2")
		return
	}
	path := filepath.Join(s.BlobDir(), name)
	fail := func(code string) { _ = stream.CloseWithReason(nodewire.ExitPrefix + code) }
	switch verb {
	case "put":
		if err := os.MkdirAll(s.BlobDir(), 0o700); err != nil {
			fail("1")
			return
		}
		size, err := nodewire.ReadSize(stream)
		if err != nil {
			fail("1")
			return
		}
		file, err := os.CreateTemp(s.BlobDir(), "."+name+"-*")
		if err != nil {
			fail("1")
			return
		}
		temp := file.Name()
		n, err := io.Copy(file, io.LimitReader(stream, size))
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err != nil || n != size {
			os.Remove(temp)
			fail("1")
			return
		}
		if err := os.Rename(temp, path); err != nil {
			os.Remove(temp)
			fail("1")
			return
		}
		log.Printf("steve-node: received blob %s (%d bytes)", name, n)
		fail("0")
	case "get":
		file, err := os.Open(path)
		if err != nil {
			fail("1")
			return
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			fail("1")
			return
		}
		if err := nodewire.WriteSize(stream, info.Size()); err != nil {
			fail("1")
			return
		}
		if _, err := io.Copy(stream, file); err != nil {
			fail("1")
			return
		}
		fail("0")
	default:
		fail("2")
	}
}

// peerGrant admits one peer connection for one blob until it expires.
type peerGrant struct {
	name    string
	expires time.Time
}

func (s *Server) validToken(token string) bool {
	if subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.Token)) == 1 {
		return true
	}
	_, ok := s.grantedName(token)
	return ok
}

func (s *Server) grantedName(token string) (string, bool) {
	s.grantsMu.Lock()
	defer s.grantsMu.Unlock()
	g, ok := s.grants[token]
	if !ok {
		return "", false
	}
	if time.Now().After(g.expires) {
		delete(s.grants, token)
		return "", false
	}
	return g.name, true
}

// grant registers a one-time peer token: "<token> <name> <seconds>".
func (s *Server) grant(stream *nodewire.Stream) {
	fields := strings.Fields(stream.Request().Command)
	if len(fields) != 3 || fields[2] == "" {
		_ = stream.CloseWithReason(nodewire.ExitPrefix + "2")
		return
	}
	seconds, err := strconv.Atoi(fields[2])
	if err != nil || seconds <= 0 || fields[1] != filepath.Base(fields[1]) {
		_ = stream.CloseWithReason(nodewire.ExitPrefix + "2")
		return
	}
	s.grantsMu.Lock()
	if s.grants == nil {
		s.grants = map[string]peerGrant{}
	}
	s.grants[fields[0]] = peerGrant{name: fields[1], expires: time.Now().Add(time.Duration(seconds) * time.Second)}
	s.grantsMu.Unlock()
	log.Printf("steve-node: granted a peer %s for %ds", fields[1], seconds)
	_ = stream.CloseWithReason(nodewire.ExitPrefix + "0")
}

// servePeer answers exactly one "get <name>" for the granted name.
func (s *Server) servePeer(ctx context.Context, socket net.Conn, hello nodewire.Hello, name string) {
	s.grantsMu.Lock()
	delete(s.grants, hello.Token)
	s.grantsMu.Unlock()
	log.Printf("steve-node: peer %q connected from %s for %s", hello.Hub, socket.RemoteAddr(), name)
	mux := nodewire.NewMux(socket, false)
	defer mux.Close()
	stream, err := mux.Accept(ctx)
	if err != nil {
		return
	}
	req := stream.Request()
	if req.Kind != nodewire.StreamBlob || req.Command != "get "+name {
		_ = stream.CloseWithReason(nodewire.ExitPrefix + "2")
		return
	}
	s.transferBlob(ctx, stream)
}

// fetch pulls a blob from a peer: "<addr> <token> <name>".
func (s *Server) fetch(ctx context.Context, stream *nodewire.Stream) {
	fields := strings.Fields(stream.Request().Command)
	fail := func(code string, err error) {
		if err != nil {
			log.Printf("steve-node: fetch: %v", err)
		}
		_ = stream.CloseWithReason(nodewire.ExitPrefix + code)
	}
	if len(fields) != 3 || fields[2] != filepath.Base(fields[2]) {
		fail("2", nil)
		return
	}
	addr, token, name := fields[0], fields[1], fields[2]
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	dialer := net.Dialer{Timeout: nodewire.HandshakeTimeout}
	socket, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		fail("1", err)
		return
	}
	defer socket.Close()
	_ = socket.SetDeadline(time.Now().Add(nodewire.HandshakeTimeout))
	if _, err := nodewire.Dial(socket, nodewire.Hello{Token: token, Hub: "peer:" + s.cfg.Name}); err != nil {
		fail("1", err)
		return
	}
	_ = socket.SetDeadline(time.Time{})
	mux := nodewire.NewMux(socket, true)
	defer mux.Close()
	blob, err := mux.Open(nodewire.OpenRequest{Kind: nodewire.StreamBlob, Command: "get " + name})
	if err != nil {
		fail("1", err)
		return
	}
	defer blob.Close()
	size, err := nodewire.ReadSize(blob)
	if err != nil {
		fail("1", err)
		return
	}
	if err := os.MkdirAll(s.BlobDir(), 0o700); err != nil {
		fail("1", err)
		return
	}
	file, err := os.CreateTemp(s.BlobDir(), "."+name+"-*")
	if err != nil {
		fail("1", err)
		return
	}
	temp := file.Name()
	n, err := io.Copy(file, io.LimitReader(blob, size))
	file.Close()
	if err != nil || n != size {
		os.Remove(temp)
		fail("1", fmt.Errorf("short read from peer: %d of %d", n, size))
		return
	}
	if err := os.Rename(temp, filepath.Join(s.BlobDir(), name)); err != nil {
		os.Remove(temp)
		fail("1", err)
		return
	}
	log.Printf("steve-node: fetched %s (%d bytes) from peer %s", name, n, addr)
	_ = stream.CloseWithReason(nodewire.ExitPrefix + "0")
}
