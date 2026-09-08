package node

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

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
	// No readable settings file means no revision to guard, which is what
	// the empty revision says; a write reports the real problem.
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
		// Shutdown closes the listener the accept loop already left; the
		// close has nothing left to report.
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
	slog.Info(fmt.Sprintf("steve-node: %s listening on %s", s.conf().Name, listener.Addr()), "node", s.conf().Name)
	go func() {
		<-ctx.Done()
		// Closing unblocks Accept, which reports the end through ctx.
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
			slog.Warn(fmt.Sprintf("steve-node: skills %s from last run could not be materialized: %v", hash[:12], err), "skills", hash)
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
	slog.Info(fmt.Sprintf("steve-node: verify %q in %s -> exit %d", req.Command, dir, code), "command", req.Command)
	closeStream(stream, fmt.Sprintf("%s%d", nodewire.ExitPrefix, code))
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

// commands is every binary this machine offers and should see start.
func (s *Server) commands() []string {
	out := make([]string, 0, len(s.conf().Harnesses)+len(s.conf().Tools))
	for _, h := range s.conf().Harnesses {
		out = append(out, h.Command)
	}
	return append(out, s.conf().Tools...)
}
