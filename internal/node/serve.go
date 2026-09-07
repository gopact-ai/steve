package node

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gopact-ai/steve/internal/ability"
	"github.com/gopact-ai/steve/internal/ledger"
	"github.com/gopact-ai/steve/internal/mcpprobe"
	"github.com/gopact-ai/steve/internal/mcpscan"
	"github.com/gopact-ai/steve/internal/nodewire"
	steveruntime "github.com/gopact-ai/steve/internal/runtime"
	"github.com/gopact-ai/steve/internal/skills"
)

// HarnessSpec is one agent runtime this node can start. The command line is
// a node-local fact — which binaries exist and which model endpoints are
// reachable is a property of this machine, not of the hub's config file.
type HarnessSpec struct {
	// Adapter names an ACP adapter from the built-in catalog, fetched at a
	// pinned version and verified before it runs. Give this or Command: a
	// command is this machine's own build, and Steve leaves it alone.
	Adapter    string   `json:"adapter,omitempty"`
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
	// Source is the file this configuration came from; settings changed
	// from the hub are written back there. Empty means nowhere.
	Source            string                        `json:"-"`
	Listener          net.Listener                  `json:"-"`
	SessionAuthorizer SessionAuthorizer             `json:"-"`
	AuthenticatedPeer func(net.Conn) (string, bool) `json:"-"`

	Name      string                 `json:"name"`
	Listen    string                 `json:"listen"`
	Token     string                 `json:"token"`
	Harnesses map[string]HarnessSpec `json:"harnesses"`
	// Capabilities are free-form tags (kept for older configs); Tools
	// are binaries to look for on PATH; MCPServers are the MCP servers
	// this machine can start for its agents; Declares are capabilities
	// nobody can check from inside a process ("network:internal",
	// "credential:prod") and so are taken on the operator's word.
	Capabilities []string           `json:"capabilities,omitempty"`
	Tools        []string           `json:"tools,omitempty"`
	MCPServers   map[string]MCPSpec `json:"mcp_servers,omitempty"`
	Declares     []string           `json:"declares,omitempty"`
	// Hubs binds a token to a hub name: a hub presenting that token must
	// call itself that, so the name the node remembers is one the token
	// vouches for. Token alone admits any name, as before.
	Hubs map[string]string `json:"hubs,omitempty"`
	// MCPBroker names a broker running as its own process; when set,
	// MCPServers here must be empty — the servers, and their secrets,
	// are the broker's.
	MCPBroker      *BrokerRef    `json:"mcp_broker,omitempty"`
	WorkspaceRoot  string        `json:"workspace_root,omitempty"`
	StateDir       string        `json:"state_dir,omitempty"`
	SessionGrace   time.Duration `json:"-"`
	FaultDropAfter time.Duration `json:"-"`
}

// BrokerRef is how the node reaches an external broker.
type BrokerRef struct {
	Socket string `json:"socket"`
	Token  string `json:"token"`
}

// MCPSpec is an MCP server as this machine can start it. Env stays on
// the machine: it goes into the agent process's environment, never into
// the advert.
type MCPSpec struct {
	Type    string            `json:"type"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// Observe is what a machine can find out about itself without starting
// anything: which configured harnesses, tools and MCP commands are on the
// PATH, what hardware is present, plus the declarations it was given.
type Observe struct {
	Harnesses map[string]HarnessSpec
	Tools     []string
	MCP       map[string]MCPSpec
	Declares  []string
	Tags      []string
	// Skills are the skills materialized into every harness home on this
	// machine, with their content hashes. SkillsKnown says the list is
	// authoritative (it is on any machine that isolates its homes).
	Skills      []skills.Entry
	SkillsKnown bool
	// MCPListed is what an external broker offers (id → transport), used
	// instead of MCP when the servers are not this process's to check;
	// MCPError says the broker could not be asked, so the kind is unknown.
	MCPListed map[string]string
	MCPError  string
	// Launch answers whether a resolved binary was seen to start, from a
	// LaunchProbe running on its own clock. Nil means existence only.
	Launch func(path string) (LaunchResult, bool)
}

// Server accepts hub connections and runs agents on this machine.
type Server struct {
	enrollmentMu         sync.Mutex
	settingsMu           sync.Mutex
	settingsFileRevision string
	// cfg is replaced whole when the hub changes the node's settings;
	// readers take the current one without locking.
	cfg atomic.Pointer[ServerConfig]
	// ctx is the serving context, kept so a broker can be started later
	// when settings first name an MCP server.
	ctx context.Context
	// generation identifies this process; sequence counts its snapshots.
	// Together they let the hub reject a snapshot that arrives out of order.
	generation int64
	seq        int64
	// launch checks that offered binaries start, in the background.
	launch *LaunchProbe
	// broker binds this machine's MCP servers for sessions: in-process
	// from node.json, or another process reached over its socket.
	broker mcpBroker
	// hub is the hub currently served. A second hub is refused: two hubs
	// placing work on one machine would each believe they own its slots.
	hubMu   sync.Mutex
	hubName string
	hubLive int

	grantsMu sync.Mutex
	grants   map[string]peerGrant

	mu          sync.Mutex
	mcpPort     int
	listener    net.Listener
	mcpListener net.Listener
	hubMux      *nodewire.Mux
	// hubWaiters is closed when a hub attaches and replaced with a fresh
	// open channel when one leaves, so anything waiting blocks on the
	// current state instead of a stale answer. Guarded by mu; read it and
	// hubMux under the same hold.
	hubWaiters chan struct{}
	// hubSeen records that a hub has attached at least once. Before that,
	// "no hub" means the node is still coming up; after it, the hub is
	// gone and will come back on its own — a caller learns more from a
	// prompt retryable error than from a wait.
	hubSeen      bool
	processMu    sync.Mutex
	processes    map[string]*agentProcess
	processWG    sync.WaitGroup
	workWG       sync.WaitGroup
	requestWG    sync.WaitGroup
	backgroundWG sync.WaitGroup
	restart      restartControl
	faultOnce    sync.Once
	sessions     *SessionService
}

func NewServer(cfg ServerConfig) *Server {
	s := &Server{mcpPort: rememberedPort(cfg), generation: nextGeneration(), launch: NewLaunchProbe(), processes: map[string]*agentProcess{}}
	s.cfg.Store(&cfg)
	s.settingsFileRevision, _ = nodeSettingsFileRevision(cfg.Source)
	return s
}

var lastGeneration atomic.Int64

func nextGeneration() int64 {
	// Microseconds distinguish fast reexecs while remaining an exact JS
	// integer in status responses. In-process fixtures can start together.
	for {
		before := lastGeneration.Load()
		next := max(time.Now().UnixMicro(), before+1)
		if lastGeneration.CompareAndSwap(before, next) {
			return next
		}
	}
}

// conf is the configuration in force.
func (s *Server) conf() ServerConfig { return *s.cfg.Load() }

// Serve blocks until ctx ends or the listener fails.
func (s *Server) Serve(ctx context.Context) error {
	if s.conf().StateDir != "" {
		unlock, err := steveruntime.AcquireLock(filepath.Join(s.conf().StateDir, "instance-control"))
		if err != nil {
			return fmt.Errorf("node instance is already running: %w", err)
		}
		defer unlock()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.ctx = ctx
	listener := s.conf().Listener
	if listener == nil {
		var err error
		listener, err = net.Listen("tcp", s.conf().Listen)
		if err != nil {
			return fmt.Errorf("listen on %s: %w", s.conf().Listen, err)
		}
	}
	s.mu.Lock()
	s.listener = listener
	s.mu.Unlock()
	var handlers sync.WaitGroup
	defer func() {
		s.restart.mu.Lock()
		s.restart.draining = true
		s.restart.mu.Unlock()
		cancel()
		_ = listener.Close()
		if s.sessions != nil {
			s.sessions.Close()
		}
		s.closeMCP()
		handlers.Wait()
		s.requestWG.Wait()
		s.workWG.Wait()
		s.stopProcesses("")
		s.processWG.Wait()
		s.backgroundWG.Wait()
	}()
	s.backgroundWG.Go(func() { s.pruneStreams(ctx) })
	log.Printf("steve-node: %s listening on %s", s.conf().Name, listener.Addr())
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	// Every harness runs in a home this node owns, never the user's own
	// ~/.codex or ~/.claude: personal MCP servers, skills and instructions
	// there would otherwise leak into every task the hub sends here.
	selectedHarnesses := make([]string, 0, len(s.conf().Harnesses))
	for id := range s.conf().Harnesses {
		selectedHarnesses = append(selectedHarnesses, id)
	}
	if err := steveruntime.PrepareSelected(s.conf().StateDir, selectedHarnesses); err != nil {
		return fmt.Errorf("prepare harness homes under %s: %w", s.conf().StateDir, err)
	}
	if hash := s.currentSkills(); hash != "" {
		if err := s.materializeSkills(hash); err != nil {
			log.Printf("steve-node: skills %s from last run could not be materialized: %v", hash[:12], err)
		}
	}
	s.backgroundWG.Go(func() { s.launch.Run(ctx, s.commands) })
	if err := s.startBroker(); err != nil {
		return err
	}
	if err := s.startSessions(ctx); err != nil {
		return err
	}
	if err := s.startRestartControl(cancel); err != nil {
		return err
	}
	for {
		socket, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				s.restart.mu.Lock()
				requested := s.restart.requested
				s.restart.mu.Unlock()
				if requested {
					return ErrRestartRequested
				}
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}
		handlers.Add(1)
		go func() { defer handlers.Done(); s.handle(ctx, socket) }()
	}
}

// Addr is the bound address, useful once Listen was ":0".
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return s.conf().Listen
	}
	return s.listener.Addr().String()
}

func (s *Server) handle(ctx context.Context, socket net.Conn) {
	defer socket.Close()
	stop := context.AfterFunc(ctx, func() { _ = socket.Close() })
	defer stop()
	_ = socket.SetDeadline(time.Now().Add(nodewire.HandshakeTimeout))
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
	}
	claimed := false
	clean := false
	claimedHub := ""
	defer func() {
		// Claim may have succeeded even when writing the advert failed.
		if claimed {
			s.release(claimedHub, clean)
		}
	}()
	hello, err := nodewire.AcceptClaim(socket, s.validToken, func(h nodewire.Hello) error {
		if _, peer := s.grantedName(h.Token); peer {
			return nil
		}
		if bound, ok := s.hubOf(h.Token); ok && bound != h.Hub {
			return fmt.Errorf("this token belongs to hub %q, not %q", bound, h.Hub)
		}
		if err := s.claim(h.Hub); err != nil {
			return err
		}
		claimed = true
		claimedHub = h.Hub
		return nil
	}, advert)
	if err != nil {
		log.Printf("steve-node: handshake from %s: %v", socket.RemoteAddr(), err)
		return
	}
	_ = socket.SetDeadline(time.Time{})
	sessionPrincipal := hello.Hub
	if authenticate := s.conf().AuthenticatedPeer; authenticate != nil {
		var ok bool
		sessionPrincipal, ok = authenticate(socket)
		if !ok || sessionPrincipal == "" {
			log.Printf("steve-node: rejected connection without authenticated peer identity")
			return
		}
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
	s.mu.Lock()
	s.hubMux = mux
	s.hubSeen = true
	if s.hubWaiters != nil {
		close(s.hubWaiters)
		s.hubWaiters = nil
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		clean = mux.Graceful() && s.hubMux == mux
		if s.hubMux == mux {
			s.hubMux = nil
		}
		s.mu.Unlock()
		if clean {
			s.stopProcesses(hello.Hub)
		}
	}()
	go func() {
		select {
		case <-ctx.Done():
			_ = mux.Close()
		case <-mux.Done():
		}
	}()
	for {
		stream, err := mux.Accept(ctx)
		if err != nil {
			log.Printf("steve-node: hub %q disconnected", hello.Hub)
			return
		}
		req := stream.Request()
		if req.Kind == nodewire.StreamRestart {
			s.requestWG.Go(func() { s.restartStream(hello.Hub, stream) })
			continue
		}
		readOnly := req.Kind == nodewire.StreamAdvert || req.Kind == nodewire.StreamInspect || (req.Kind == nodewire.StreamConfig && (req.Command == "get" || req.Command == "discover-agents"))
		var done func()
		if !readOnly {
			done, err = s.beginWork()
			if err != nil {
				_ = stream.CloseWithReason(err.Error())
				continue
			}
		}
		s.requestWG.Go(func() {
			if done != nil {
				defer done()
			}
			switch req.Kind {
			case nodewire.StreamNodeSessions:
				s.sessionStream(ctx, sessionPrincipal, stream)
			case nodewire.StreamExec:
				s.runCommand(ctx, stream)
			case nodewire.StreamArtifact:
				s.runArtifact(ctx, stream)
			case nodewire.StreamFiles:
				s.runFiles(ctx, stream)
			case nodewire.StreamAdvert:
				s.sendAdvert(stream)
			case nodewire.StreamBlob:
				s.transferBlob(ctx, stream)
			case nodewire.StreamGrant:
				s.grant(stream)
			case nodewire.StreamFetch:
				s.fetch(ctx, stream)
			case nodewire.StreamAdmit:
				s.admit(stream)
			case nodewire.StreamSkills:
				s.applySkills(stream)
			case nodewire.StreamRelease:
				s.releaseAttempt(stream)
			case nodewire.StreamConfig:
				s.configure(stream)
			case nodewire.StreamInspect:
				s.inspect(ctx, stream)
			case nodewire.StreamMCPProbe:
				s.mcpProbe(ctx, stream)
			default:
				if stream.Request().Kind == nodewire.StreamACP {
					s.injectDrop(ctx, mux)
				}
				s.runAgent(ctx, stream)
			}
		})
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
		dir = s.conf().WorkspaceRoot
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

func (s *Server) processDir(spec HarnessSpec) string {
	if spec.ProcessDir != "" {
		return spec.ProcessDir
	}
	return s.conf().WorkspaceRoot
}

func (s *Server) nextSequence() int64 {
	s.hubMu.Lock()
	defer s.hubMu.Unlock()
	s.seq++
	return s.seq
}

// claim admits the instance's owner by name. Disconnecting or losing a hub
// never grants ownership to another; adoption changes the persisted owner
// while the instance is stopped.
func (s *Server) claim(hub string) error {
	s.hubMu.Lock()
	defer s.hubMu.Unlock()
	if s.hubName != "" && s.hubName != hub {
		return fmt.Errorf("this node belongs to hub %q; stop this instance and explicitly adopt %q", s.hubName, hub)
	}
	// The disk record preserves ownership across restarts. Released is
	// evidence that the old processes stopped, not permission to take over.
	if s.hubLive == 0 {
		owner, err := s.readOwner()
		if err != nil {
			return err
		}
		if owner.Hub != "" && owner.Hub != hub {
			return fmt.Errorf("this node belongs to hub %q; stop this instance and explicitly adopt %q", owner.Hub, hub)
		}
	}
	if err := s.writeOwner(hubOwner{Hub: hub, LastSeen: time.Now().UTC()}); err != nil {
		return err
	}
	s.hubName = hub
	s.hubLive++
	return nil
}

func (s *Server) release(hub string, clean bool) {
	if clean {
		clean = s.processesStopped(hub)
	}
	s.hubMu.Lock()
	defer s.hubMu.Unlock()
	if s.hubName == hub && s.hubLive > 0 {
		s.hubLive--
		if s.hubLive == 0 {
			if err := s.writeOwner(hubOwner{Hub: hub, LastSeen: time.Now().UTC(), Released: clean}); err != nil {
				log.Printf("node: persist owner release: %v", err)
			}
		}
	}
}

// hubOwner binds an instance to its hub across disconnects and restarts.
type hubOwner struct {
	Hub      string    `json:"hub"`
	LastSeen time.Time `json:"last_seen"`
	// Released records a clean disconnect with stopped processes. It can
	// supply stop evidence to offline adoption but never clears ownership.
	Released bool `json:"released,omitempty"`
}

func (s *Server) ownerPath() string { return filepath.Join(s.conf().StateDir, "hub.json") }

func (s *Server) owner() hubOwner { o, _ := s.readOwner(); return o }
func (s *Server) readOwner() (hubOwner, error) {
	var o hubOwner
	if s.conf().StateDir == "" {
		return o, nil
	}
	raw, err := os.ReadFile(s.ownerPath())
	if os.IsNotExist(err) {
		return o, nil
	}
	if err != nil {
		return o, err
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return o, fmt.Errorf("node ownership record unreadable: %w", err)
	}
	return o, nil
}

func (s *Server) writeOwner(o hubOwner) error {
	if s.conf().StateDir == "" {
		return nil
	}
	raw, err := json.Marshal(o)
	if err != nil {
		return err
	}
	return (&ledger.FileDocument{Path: s.ownerPath()}).Save(raw)
}

func (s *Server) processesStopped(hub string) bool {
	if s.sessions != nil && !s.sessions.processesStopped() {
		return false
	}
	s.processMu.Lock()
	defer s.processMu.Unlock()
	for _, p := range s.processes {
		if p.owner != hub {
			continue
		}
		p.mu.Lock()
		ended := p.exit != ""
		p.mu.Unlock()
		if !ended {
			return false
		}
	}
	return true
}

// touchOwner marks the serving hub as seen now; the hub's minute refresh
// keeps this current while the connection lives.
func (s *Server) touchOwner() {
	s.hubMu.Lock()
	defer s.hubMu.Unlock()
	if s.hubLive > 0 {
		if err := s.writeOwner(hubOwner{Hub: s.hubName, LastSeen: time.Now().UTC()}); err != nil {
			log.Printf("node: persist owner heartbeat: %v", err)
		}
	}
}

// Adopt assigns a stopped instance to the named hub. Handshakes from any
// other hub remain refused, even before the new owner first connects.
func Adopt(stateDir, hub string) error { return AdoptWithEvidence(stateDir, hub, "operator", "") }

// AdoptWithEvidence requires the instance stopped. Unfinished stream journals
// additionally require the operator's explicit physical-stop verification.
// This records that statement; it does not pretend to verify another process.
func AdoptWithEvidence(stateDir, hub, actor, evidence string) error {
	if hub == "" || stateDir == "" {
		return errors.New("adopt requires node state directory and hub identity")
	}
	unlock, err := steveruntime.AcquireLock(filepath.Join(stateDir, "instance-control"))
	if err != nil {
		return fmt.Errorf("stop the node instance before adoption: %w", err)
	}
	defer unlock()
	streams, err := os.ReadDir(filepath.Join(stateDir, "streams"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var unknown []string
	for _, stream := range streams {
		if !stream.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(stateDir, "streams", stream.Name(), "ended")); err != nil {
			unknown = append(unknown, stream.Name())
		}
	}
	if len(unknown) > 0 && strings.TrimSpace(evidence) == "" {
		return fmt.Errorf("process streams %v have no verified exit; --evidence must record operator verification after physically stopping them", unknown)
	}
	s := &Server{}
	s.cfg.Store(&ServerConfig{StateDir: stateDir})
	previous, err := s.readOwner()
	if err != nil {
		return err
	}
	if previous.Hub != "" && previous.Hub != hub && !previous.Released && strings.TrimSpace(evidence) == "" {
		return errors.New("previous owner did not cleanly release; explicit physical stop evidence is required")
	}
	record := struct {
		From, To, Actor, Evidence string
		At                        time.Time
		UnknownStreams            []string
	}{previous.Hub, hub, actor, evidence, time.Now().UTC(), unknown}
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if err := (&ledger.FileDocument{Path: filepath.Join(stateDir, "last-adoption.json")}).Save(raw); err != nil {
		return err
	}
	return s.writeOwner(hubOwner{Hub: hub, LastSeen: record.At, Released: true})
}

// advert reports what this machine can honestly do. Harnesses whose command
// is not on this node's PATH are listed as missing rather than omitted: a
// roster that hides what is broken sends the hub hunting for a node that
// silently vanished.
func (s *Server) advert() nodewire.Advert {
	adv := Advertise(s.conf().Name, s.conf().Harnesses, s.conf().Capabilities)
	adv.Snapshot = s.snapshot()
	adv.Features = nodewire.Features()
	if s.sessions != nil {
		adv.Features = append(adv.Features, nodewire.FeatureNodeSessions)
	}
	s.restart.mu.Lock()
	if s.restart.enabled && s.conf().StateDir != "" {
		adv.Features = append(adv.Features, nodewire.FeatureRestart)
	}
	s.restart.mu.Unlock()
	adv.SessionGraceMS = s.sessionGrace().Milliseconds()
	adv.WorkspaceRoot = s.conf().WorkspaceRoot
	adv.StateDir = s.conf().StateDir
	adv.Skills = s.currentSkills()
	adv.OwnSkills = OwnSkills(5 * time.Minute)
	adv.OwnMCP = OwnMCP(5 * time.Minute)
	adv.Health = CheckHealth(s.conf().WorkspaceRoot, s.conf().StateDir)
	// The reverse messaging port is part of every advert, not only the
	// handshake's: a refresh that dropped it would leave the hub thinking
	// this machine's agents cannot reach it.
	s.mu.Lock()
	adv.MCPPort = s.mcpPort
	s.mu.Unlock()
	return adv
}

// ownMCP is this machine's scan of its coding agents' own MCP servers,
// shapes only; the advert carries it.
var ownMCP mcpscan.Local

// OwnMCP is the machine's own MCP servers, rescanned when older than maxAge.
func OwnMCP(maxAge time.Duration) []nodewire.OwnMCP {
	found := ownMCP.Get(maxAge)
	out := make([]nodewire.OwnMCP, 0, len(found))
	for _, f := range found {
		out = append(out, nodewire.OwnMCP{Name: f.Name, Source: f.Source, Scope: f.Scope, Type: f.Type, Command: f.Command, Args: f.Args, URL: f.URL, EnvKeys: f.EnvKeys, HeaderKeys: f.HeaderKeys})
	}
	return out
}

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
	reply := func(r nodewire.MCPProbeReply) { _ = json.NewEncoder(stream).Encode(r) }
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
	defer func() { _, _ = s.broker.Release(context.Background(), attempt) }()
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

// ownSkills is this machine's scan of its AI tools' own skills; the
// advert carries it, so the hub's refresh loop keeps its copy fresh.
var ownSkills skills.Local

// OwnSkills is the machine's own skills, rescanned when older than maxAge.
func OwnSkills(maxAge time.Duration) []nodewire.OwnSkill {
	found := ownSkills.Get(maxAge)
	out := make([]nodewire.OwnSkill, 0, len(found))
	for _, f := range found {
		out = append(out, nodewire.OwnSkill{Name: f.Name, Path: f.Path, Title: f.Title, Description: f.Description})
	}
	return out
}

// SkillsDir holds materialized bundles, one directory per hash, and
// "current" naming the one the harness homes link to.
func (s *Server) SkillsDir() string { return filepath.Join(s.conf().StateDir, "skills") }

func (s *Server) currentSkills() string {
	b, err := os.ReadFile(filepath.Join(s.SkillsDir(), "current"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (s *Server) skillEntries() []skills.Entry {
	hash := s.currentSkills()
	if hash == "" {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(s.SkillsDir(), hash, ".manifest.json"))
	if err != nil {
		return nil
	}
	var entries []skills.Entry
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil
	}
	return entries
}

// applySkills takes the bundle the hub just put in the blob directory,
// checks it is the bundle it claims to be, unpacks it next to the other
// bundles and links every skill into every harness home. Old bundles go
// once the new one is current. New sessions see the new skills; running
// ones keep what they started with until the hub restarts them.
func (s *Server) applySkills(stream *nodewire.Stream) {
	defer stream.Close()
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	verb, hash, _ := strings.Cut(stream.Request().Command, " ")
	hash = strings.TrimSpace(hash)
	fail := func(code string, err error) {
		log.Printf("steve-node: skills %s: %v", hash, err)
		fmt.Fprintln(stream, err.Error())
		_ = stream.CloseWithReason(nodewire.ExitPrefix + code)
	}
	if verb != "apply" || hash == "" || hash != filepath.Base(hash) {
		fail("2", fmt.Errorf("bad request %q", stream.Request().Command))
		return
	}
	blob := filepath.Join(s.BlobDir(), "skills-"+hash+".tar")
	data, err := os.ReadFile(blob)
	if err != nil {
		fail("1", fmt.Errorf("bundle not received: %w", err))
		return
	}
	if got := skills.HashOf(data); got != hash {
		fail("1", fmt.Errorf("bundle hash is %s, not %s", got[:12], hash[:12]))
		return
	}
	dir := filepath.Join(s.SkillsDir(), hash)
	staging := dir + ".staging"
	_ = os.RemoveAll(staging)
	entries, err := skills.Unpack(data, staging)
	if err != nil {
		_ = os.RemoveAll(staging)
		fail("1", fmt.Errorf("unpack: %w", err))
		return
	}
	manifest, _ := json.Marshal(entries)
	if err := os.WriteFile(filepath.Join(staging, ".manifest.json"), manifest, 0o600); err != nil {
		fail("1", err)
		return
	}
	_ = os.RemoveAll(dir)
	if err := os.Rename(staging, dir); err != nil {
		fail("1", err)
		return
	}
	if err := s.materializeSkills(hash); err != nil {
		fail("1", err)
		return
	}
	if err := os.WriteFile(filepath.Join(s.SkillsDir(), "current"), []byte(hash+"\n"), 0o600); err != nil {
		fail("1", err)
		return
	}
	_ = os.Remove(blob)
	if old, err := os.ReadDir(s.SkillsDir()); err == nil {
		for _, e := range old {
			if e.IsDir() && e.Name() != hash {
				_ = os.RemoveAll(filepath.Join(s.SkillsDir(), e.Name()))
			}
		}
	}
	log.Printf("steve-node: skills %s materialized: %d skills", hash[:12], len(entries))
	_ = stream.CloseWithReason(nodewire.ExitPrefix + "0")
}

// materializeSkills links every skill of the bundle into every harness
// home, replacing whatever was linked before.
func (s *Server) materializeSkills(hash string) error {
	selected := make([]string, 0, len(s.conf().Harnesses))
	for id := range s.conf().Harnesses {
		selected = append(selected, id)
	}
	return s.materializeSkillsFor(hash, selected)
}

func (s *Server) materializeSkillsFor(hash string, selected []string) error {
	dir := filepath.Join(s.SkillsDir(), hash)
	names, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, dest := range steveruntime.SelectedSkillDests(s.conf().StateDir, selected) {
		if err := os.MkdirAll(dest, 0o700); err != nil {
			return err
		}
		old, err := os.ReadDir(dest)
		if err != nil {
			return err
		}
		for _, e := range old {
			if err := os.RemoveAll(filepath.Join(dest, e.Name())); err != nil {
				return err
			}
		}
		for _, n := range names {
			if !n.IsDir() {
				continue
			}
			if err := os.Symlink(filepath.Join(dir, n.Name()), filepath.Join(dest, n.Name())); err != nil {
				return fmt.Errorf("link skill %q: %w", n.Name(), err)
			}
		}
	}
	return nil
}

// snapshot observes this machine now, as the next revision.
func (s *Server) snapshot() *ability.Snapshot {
	o := Observe{Harnesses: s.conf().Harnesses, Tools: s.conf().Tools, MCP: s.conf().MCPServers, Declares: s.conf().Declares, Tags: s.conf().Capabilities, Launch: s.launch.Lookup, Skills: s.skillEntries(), SkillsKnown: true}
	if rb, ok := s.broker.(remoteBroker); ok {
		// The servers are the broker's: what it lists is what there is,
		// and the broker vouches for them, not a PATH lookup here.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		listed, err := rb.List(ctx)
		if err != nil {
			log.Printf("steve-node: mcp broker: %v", err)
			o.MCPError = err.Error()
		} else {
			o.MCPListed = listed
		}
	}
	return Snapshot(s.conf().Name, s.generation, s.nextSequence(), o)
}

// commands is every binary this machine offers and should see start.
func (s *Server) commands() []string {
	out := make([]string, 0, len(s.conf().Harnesses)+len(s.conf().Tools))
	for _, h := range s.conf().Harnesses {
		out = append(out, h.Command)
	}
	return append(out, s.conf().Tools...)
}

// admit is the node's final word before an attempt runs here: the hub
// placed on a snapshot it accepted earlier; the node re-checks the clauses
// it owns on an observation taken now and says what it found, with the
// revision it found it on. A refusal is a verdict, not an error.
func (s *Server) admit(stream *nodewire.Stream) {
	defer stream.Close()
	var req nodewire.AdmitRequest
	if err := json.NewDecoder(stream).Decode(&req); err != nil {
		_ = json.NewEncoder(stream).Encode(nodewire.AdmitReply{Error: "read request: " + err.Error()})
		return
	}
	snap := s.snapshot()
	if snap == nil {
		_ = json.NewEncoder(stream).Encode(nodewire.AdmitReply{Error: "this node cannot observe itself"})
		return
	}
	now := time.Now().UTC()
	m := ability.Match(req.Requirement, snap, req.Harness, now)
	adm := ability.AdmissionOf(m, snap, ability.SourceNode, now)
	// What the session will use is bound now, or the admission fails: a
	// server this machine does not have, or cannot start for a session,
	// is a definite no.
	var bindings []ability.Binding
	for _, id := range req.Uses {
		atom := "mcp:" + id
		if s.broker == nil {
			adm.Verdict, adm.Code = ability.False, ability.CodeAbsent
			adm.Atoms = append(adm.Atoms, ability.AtomResult{Atom: atom, Verdict: ability.False, Code: ability.CodeAbsent, Detail: "this node has no MCP servers"})
			continue
		}
		if adm.Verdict != ability.True {
			continue
		}
		d, err := s.broker.Bind(context.Background(), id, req.Attempt, req.Harness)
		switch {
		case errors.Is(err, ErrNoSuchServer):
			adm.Verdict, adm.Code = ability.False, ability.CodeAbsent
			adm.Atoms = append(adm.Atoms, ability.AtomResult{Atom: atom, Verdict: ability.False, Code: ability.CodeAbsent})
		case errors.Is(err, ErrUnbindable):
			adm.Verdict, adm.Code = ability.False, ability.CodeUnavailable
			adm.Atoms = append(adm.Atoms, ability.AtomResult{Atom: atom, Verdict: ability.False, Code: ability.CodeUnavailable, Detail: err.Error()})
		case err != nil:
			_ = json.NewEncoder(stream).Encode(nodewire.AdmitReply{Error: "bind " + id + ": " + err.Error()})
			return
		default:
			bindings = append(bindings, d)
			adm.Bound = append(adm.Bound, id)
		}
	}
	if adm.Verdict != ability.True {
		bindings, adm.Bound = nil, nil
	}
	log.Printf("steve-node: admission for attempt %s (%s): %s at %d/%d, bound %v", req.Attempt, req.Harness, adm.Verdict, snap.Generation, snap.Sequence, adm.Bound)
	if err := json.NewEncoder(stream).Encode(nodewire.AdmitReply{Admission: adm, Bindings: bindings, Nonce: req.Nonce}); err != nil {
		log.Printf("steve-node: admission reply: %v", err)
	}
}

// Snapshot observes the machine into an ability snapshot. Everything that
// can be checked is checked (existence on PATH; hardware present); the
// rest is declared and says so. Coverage names the kinds that were fully
// checked, so "not listed" means "absent" only for those.
func Snapshot(name string, generation, sequence int64, o Observe) *ability.Snapshot {
	now := time.Now().UTC()
	s := &ability.Snapshot{
		Schema: ability.Schema, Node: name, Generation: generation, Sequence: sequence, GeneratedAt: now,
		Coverage: map[ability.Kind]ability.Coverage{
			ability.Harness: ability.Complete, ability.Tool: ability.Complete, ability.MCP: ability.Complete,
			ability.Hardware: ability.Partial, ability.Network: ability.Complete, ability.Credential: ability.Complete,
			ability.Tag: ability.Complete, ability.Model: ability.Partial, ability.Skill: ability.Unsupported, ability.A2A: ability.Unsupported,
		},
		Features: nodewire.Features(), Source: "node",
	}
	observed := func(kind ability.Kind, id, cmd string) ability.Capability {
		c := ability.Capability{Kind: kind, ID: id, Assurance: ability.Existence}
		path, err := exec.LookPath(cmd)
		if err != nil {
			c.Evidence = []ability.Evidence{{Kind: ability.Observed, Method: "path", OK: false, Result: fmt.Sprintf("%q not on this node's PATH", cmd), At: now}}
			c.Detail = fmt.Sprintf("%q not on this node's PATH", cmd)
			return c
		}
		c.Evidence = []ability.Evidence{{Kind: ability.Observed, Method: "path", OK: true, Result: path, At: now}}
		// Being on PATH is existence; having started is launchable. The
		// launch check ran on its own clock, so it carries its own time,
		// and a binary that would not start makes the entry unavailable.
		if o.Launch != nil {
			if r, ok := o.Launch(path); ok {
				c.Evidence = append(c.Evidence, ability.Evidence{Kind: ability.Observed, Method: "launch", OK: r.OK, Result: r.Result, At: r.At})
				if r.OK {
					c.Assurance = ability.Launchable
					c.Version = r.Version
				} else {
					c.Detail = "does not start: " + r.Result
				}
			}
		}
		return c
	}
	ids := make([]string, 0, len(o.Harnesses))
	for id := range o.Harnesses {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		s.Offers = append(s.Offers, observed(ability.Harness, id, o.Harnesses[id].Command))
	}
	for _, tool := range o.Tools {
		s.Offers = append(s.Offers, observed(ability.Tool, tool, tool))
	}
	if o.MCPError != "" {
		s.Coverage[ability.MCP] = ability.Errored
	}
	listed := make([]string, 0, len(o.MCPListed))
	for id := range o.MCPListed {
		listed = append(listed, id)
	}
	sort.Strings(listed)
	for _, id := range listed {
		for _, h := range ids {
			s.Offers = append(s.Offers, ability.Capability{Kind: ability.MCP, ID: id, Scope: h, Assurance: ability.Existence, Attrs: map[string]string{"transport": o.MCPListed[id]},
				Evidence: []ability.Evidence{{Kind: ability.Observed, Method: "broker", OK: true, At: now}}})
		}
	}
	names := make([]string, 0, len(o.MCP))
	for id := range o.MCP {
		names = append(names, id)
	}
	sort.Strings(names)
	for _, id := range names {
		spec := o.MCP[id]
		// MCP servers are offered per harness scope? They are the machine's;
		// a harness starts them. Until a broker exists they are observed
		// only and never scheduled, so scope is every configured harness.
		for _, h := range ids {
			var c ability.Capability
			switch spec.Type {
			case "stdio", "":
				c = observed(ability.MCP, id, spec.Command)
			case "http", "sse":
				// Reached through the node's loopback proxy, which adds the
				// configured headers; the URL itself is checked for shape.
				parsed, perr := url.ParseRequestURI(spec.URL)
				ok := perr == nil && parsed.Host != "" && (parsed.Scheme == "http" || parsed.Scheme == "https")
				c = ability.Capability{Kind: ability.MCP, ID: id, Assurance: ability.Existence, Attrs: map[string]string{"transport": spec.Type},
					Evidence: []ability.Evidence{{Kind: ability.Observed, Method: "url", OK: ok, At: now}}}
				if !ok {
					c.Detail = "url is not http(s)"
				}
			default:
				c = ability.Capability{Kind: ability.MCP, ID: id, Evidence: []ability.Evidence{{Kind: ability.Observed, Method: "config", OK: false, Result: "unknown type " + spec.Type, At: now}}, Detail: "unknown type " + spec.Type}
			}
			c.Scope = h
			s.Offers = append(s.Offers, c)
		}
	}
	s.Offers = append(s.Offers, hardware(now)...)
	// Skills are what this machine materialized into its harness homes:
	// present for every harness, addressed by content. A machine that
	// isolates its homes knows the whole list, so the kind is covered.
	if o.SkillsKnown {
		s.Coverage[ability.Skill] = ability.Complete
		for _, sk := range o.Skills {
			for _, h := range ids {
				s.Offers = append(s.Offers, ability.Capability{Kind: ability.Skill, ID: sk.Name, Scope: h, Assurance: ability.Existence,
					Version:  &ability.Version{Scheme: "opaque", Value: sk.Hash},
					Evidence: []ability.Evidence{{Kind: ability.Observed, Method: "materialized", OK: true, Result: sk.Hash[:12], At: now}}})
			}
		}
	}
	for _, d := range o.Declares {
		atom, err := ability.ParseAtom(d)
		if err != nil || atom.ID == "" {
			continue
		}
		s.Offers = append(s.Offers, ability.Capability{Kind: atom.Kind, ID: atom.ID, Evidence: []ability.Evidence{{Kind: ability.Declared, Method: "config", OK: true}}, Detail: "declared in config"})
	}
	for _, tag := range o.Tags {
		s.Offers = append(s.Offers, ability.Capability{Kind: ability.Tag, ID: tag, Evidence: []ability.Evidence{{Kind: ability.Declared, Method: "config", OK: true}}})
	}
	if err := ability.Validate(s); err != nil {
		// A snapshot this machine cannot even validate is not reported; the
		// hub sees an old-style advert and treats coverage as partial.
		log.Printf("steve-node: snapshot invalid, not reported: %v", err)
		return nil
	}
	return s
}

// hardware is what can be seen without root: CPUs, the architecture, and
// whether an NVIDIA GPU is present.
func hardware(now time.Time) []ability.Capability {
	seen := func(id, detail string, attrs map[string]string) ability.Capability {
		return ability.Capability{Kind: ability.Hardware, ID: id, Assurance: ability.Existence, Attrs: attrs, Detail: detail,
			Evidence: []ability.Evidence{{Kind: ability.Observed, Method: "probe", OK: true, At: now}}}
	}
	out := []ability.Capability{
		seen("cpu", "", map[string]string{"count": strconv.Itoa(runtime.NumCPU())}),
		seen(runtime.GOARCH, "", nil),
	}
	if _, err := exec.LookPath("nvidia-smi"); err == nil {
		out = append(out, seen("gpu", "nvidia-smi on PATH", map[string]string{"vendor": "nvidia"}))
	} else if _, err := os.Stat("/dev/nvidia0"); err == nil {
		out = append(out, seen("gpu", "/dev/nvidia0", map[string]string{"vendor": "nvidia"}))
	}
	return out
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
	git := gitVersion()
	return nodewire.Advert{
		Node: name, OS: runtime.GOOS, Arch: runtime.GOARCH,
		BuildVersion: nodewire.Version(), Hostname: hostname, IPs: ips,
		Harnesses: harnesses, Capabilities: caps,
		Git: git, GitMinimum: nodewire.MinimumGitVersion, GitWarning: nodewire.GitWarning(git),
	}
}

// sendAdvert answers a hub asking "check yourself again": the same advert
// the handshake carried, computed now, so a harness installed since then
// is seen without dropping the connection.
func (s *Server) sendAdvert(stream *nodewire.Stream) {
	defer stream.Close()
	s.touchOwner()
	if err := json.NewEncoder(stream).Encode(s.advert()); err != nil {
		log.Printf("steve-node: send advert: %v", err)
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
	path := portFile(s.conf())
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "--version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(string(out), "git version "))
}

// BlobDir is where the hub's files land on this node.
func (s *Server) BlobDir() string { return filepath.Join(s.conf().StateDir, "blobs") }

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
	if s.conf().Token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.conf().Token)) == 1 {
		return true
	}
	if hub, _ := s.hubOf(token); hub != "" {
		return true
	}
	_, ok := s.grantedName(token)
	return ok
}

// hubOf is the hub name a token vouches for, from the hubs table; "" when
// the token is the shared one or unknown.
func (s *Server) hubOf(token string) (string, bool) {
	for name, t := range s.conf().Hubs {
		if t != "" && subtle.ConstantTimeCompare([]byte(token), []byte(t)) == 1 {
			return name, true
		}
	}
	return "", false
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
	done, err := s.beginWork()
	if err != nil {
		_ = stream.CloseWithReason(err.Error())
		return
	}
	defer done()
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
	if _, err := nodewire.Dial(socket, nodewire.Hello{Token: token, Hub: "peer:" + s.conf().Name}); err != nil {
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
