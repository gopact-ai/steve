// Package acphost runs one ACP agent subprocess (e.g. codex-acp) and exposes
// a session-oriented prompt API on top of the ACP client connection.
package acphost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/permission"
)

var ErrResumeUnsupported = errors.New("agent does not support session resume")
var ErrSessionBusy = errors.New("session already has a running turn")

type Config struct {
	Command    string
	Args       []string
	ProcessDir string
	Env        []string
	Permission *permission.Broker
}

type SessionConfig struct {
	Workdir    string
	MCPServers []acp.MCPServer
}

// Host owns the agent subprocess and its ACP connection. It restarts the
// process lazily on the next call after a crash.
type Host struct {
	cfg Config

	mu           sync.Mutex
	cmd          *exec.Cmd
	conn         *acp.Conn
	caller       *acp.AgentCaller
	stdin        io.WriteCloser
	alive        bool
	exited       chan struct{}
	collectors   map[acp.SessionID]*collector
	capabilities *acp.AgentCapabilities
	sessions     map[acp.SessionID]bool
	opening      map[acp.SessionID]uint64
	active       map[acp.SessionID]uint64
	generation   uint64
}

func New(cfg Config) *Host {
	if cfg.Permission == nil {
		cfg.Permission, _ = permission.New("deny")
	}
	return &Host{
		cfg: cfg, collectors: map[acp.SessionID]*collector{}, sessions: map[acp.SessionID]bool{},
		opening: map[acp.SessionID]uint64{}, active: map[acp.SessionID]uint64{},
	}
}

// collector accumulates streamed session updates for one in-flight prompt.
type collector struct {
	mu         sync.Mutex
	text       strings.Builder
	activity   []string
	progress   func(string)
	generation uint64
}

func (c *collector) handle(u acp.SessionUpdate) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch u.SessionUpdate {
	case acp.SessionUpdateTypeAgentMessageChunk:
		if cb, ok := u.Content.(acp.ContentBlock); ok && cb.Type == acp.ContentBlockTypeText {
			c.text.WriteString(cb.Text)
		}
	case acp.SessionUpdateTypeToolCall:
		title := ""
		if u.Title != nil {
			title = *u.Title
		}
		line := fmt.Sprintf("⚙ %s", title)
		c.activity = append(c.activity, line)
		if c.progress != nil {
			c.progress(line)
		}
	}
}

func (c *collector) result() (string, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.text.String(), c.activity
}

// clientHandler implements the ACP client side for the agent subprocess.
type clientHandler struct {
	h          *Host
	generation uint64
}

func (ch *clientHandler) Update(_ context.Context, n *acp.SessionNotification) error {
	ch.h.mu.Lock()
	col := ch.h.collectors[n.SessionID]
	ch.h.mu.Unlock()
	if col != nil && col.generation == ch.generation {
		col.handle(n.Update)
	}
	return nil
}

func (ch *clientHandler) RequestPermission(_ context.Context, req *acp.RequestPermissionRequest) (*acp.RequestPermissionResponse, error) {
	outcome := ch.h.cfg.Permission.Decide(req.Options)
	title := ""
	if req.ToolCall.Title != nil {
		title = *req.ToolCall.Title
	}
	log.Printf("acphost: permission request %q -> %s", title, outcome.Outcome)
	return &acp.RequestPermissionResponse{Outcome: outcome}, nil
}

// ensureStarted launches the subprocess and performs ACP initialize if needed.
func (h *Host) ensureStarted(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.alive {
		return nil
	}
	processDir := h.cfg.ProcessDir
	if processDir == "" {
		processDir = "."
	}
	if err := os.MkdirAll(processDir, 0o755); err != nil {
		return fmt.Errorf("create agent workdir: %w", err)
	}
	cmd := exec.Command(h.cfg.Command, h.cfg.Args...)
	cmd.Dir = processDir
	cmd.Env = append(os.Environ(), h.cfg.Env...)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start agent %q: %w", h.cfg.Command, err)
	}
	generation := h.generation + 1
	conn, err := acp.NewClient(stdout, stdin, func(caller *acp.AgentCaller) acp.ClientHandler {
		h.caller = caller
		return &clientHandler{h: h, generation: generation}
	})
	if err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("acp client: %w", err)
	}
	exited := make(chan struct{})
	h.cmd = cmd
	h.conn = conn
	h.stdin = stdin
	h.alive = true
	h.exited = exited
	h.generation = generation

	// Sole waiter for this process; broadcasts exit before taking the lock
	// so shutdownLocked can wait on `exited` while holding h.mu.
	go func() {
		<-conn.Done()
		connErr := conn.Err()
		_ = cmd.Wait()
		close(exited)
		h.mu.Lock()
		if h.cmd == cmd {
			h.alive = false
			h.collectors = map[acp.SessionID]*collector{}
			h.sessions = map[acp.SessionID]bool{}
			h.opening = map[acp.SessionID]uint64{}
			h.active = map[acp.SessionID]uint64{}
			h.capabilities = nil
		}
		h.mu.Unlock()
		if connErr != nil && !errors.Is(connErr, io.EOF) {
			log.Printf("acphost: connection closed: %v", connErr)
		} else {
			log.Printf("acphost: agent process exited")
		}
	}()

	initCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	resp, err := h.caller.Initialize(initCtx, &acp.InitializeRequest{
		ProtocolVersion:    acp.ProtocolVersionV1,
		ClientInfo:         &acp.Implementation{Name: "steve", Version: "0.1.0"},
		ClientCapabilities: &acp.ClientCapabilities{},
	})
	if err != nil {
		h.shutdownLocked()
		return fmt.Errorf("initialize: %w", err)
	}
	name := "unknown"
	if resp.AgentInfo != nil {
		name = fmt.Sprintf("%s %s", resp.AgentInfo.Name, resp.AgentInfo.Version)
	}
	h.capabilities = resp.AgentCapabilities
	log.Printf("acphost: connected to agent %s (protocol v%d)", name, resp.ProtocolVersion)
	return nil
}

func (h *Host) OpenSession(ctx context.Context, sessionID acp.SessionID, cfg SessionConfig) (acp.SessionID, uint64, error) {
	if err := h.ensureStarted(ctx); err != nil {
		return "", 0, err
	}
	h.mu.Lock()
	generation := h.generation
	if h.sessions[sessionID] {
		h.mu.Unlock()
		return sessionID, generation, nil
	}
	caller, capabilities := h.caller, h.capabilities
	if sessionID != "" {
		if h.opening[sessionID] != 0 {
			h.mu.Unlock()
			return "", 0, fmt.Errorf("session %q is already being opened", sessionID)
		}
		h.opening[sessionID] = generation
		defer func() {
			h.mu.Lock()
			if h.opening[sessionID] == generation {
				delete(h.opening, sessionID)
			}
			h.mu.Unlock()
		}()
	}
	h.mu.Unlock()
	if err := validateMCPServers(capabilities, cfg.MCPServers); err != nil {
		return "", 0, err
	}
	if sessionID != "" {
		request := acp.LoadSessionRequest{SessionID: sessionID, Cwd: cfg.Workdir, MCPServers: cfg.MCPServers}
		switch {
		case capabilities != nil && capabilities.LoadSession:
			if _, err := caller.LoadSession(ctx, &request); err != nil {
				return "", 0, fmt.Errorf("session/load: %w", err)
			}
		case capabilities != nil && capabilities.SessionCapabilities != nil && capabilities.SessionCapabilities.Resume != nil:
			if _, err := caller.ResumeSession(ctx, &acp.ResumeSessionRequest{
				SessionID: sessionID, Cwd: cfg.Workdir, MCPServers: cfg.MCPServers,
			}); err != nil {
				return "", 0, fmt.Errorf("session/resume: %w", err)
			}
		default:
			return "", 0, ErrResumeUnsupported
		}
		h.mu.Lock()
		if !h.alive || h.generation != generation {
			h.mu.Unlock()
			return "", 0, fmt.Errorf("agent process changed while opening session")
		}
		h.sessions[sessionID] = true
		h.mu.Unlock()
		return sessionID, generation, nil
	}
	resp, err := caller.NewSession(ctx, &acp.NewSessionRequest{
		Cwd:        cfg.Workdir,
		MCPServers: cfg.MCPServers,
	})
	if err != nil {
		return "", 0, fmt.Errorf("session/new: %w", err)
	}
	h.mu.Lock()
	if !h.alive || h.generation != generation {
		h.mu.Unlock()
		return "", 0, fmt.Errorf("agent process changed while opening session")
	}
	h.sessions[resp.SessionID] = true
	h.mu.Unlock()
	return resp.SessionID, generation, nil
}

// Prompt sends one user turn and blocks until the agent finishes it,
// returning the aggregated assistant text and tool-activity lines.
func (h *Host) Prompt(ctx context.Context, sid acp.SessionID, generation uint64, text string, progress func(string)) (string, []string, error) {
	if err := h.ensureStarted(ctx); err != nil {
		return "", nil, err
	}
	h.mu.Lock()
	if h.generation != generation {
		h.mu.Unlock()
		return "", nil, fmt.Errorf("agent process changed before prompt")
	}
	if h.active[sid] != 0 {
		h.mu.Unlock()
		return "", nil, ErrSessionBusy
	}
	caller := h.caller
	col := &collector{progress: progress, generation: generation}
	h.collectors[sid] = col
	h.active[sid] = generation
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		if h.collectors[sid] == col {
			delete(h.collectors, sid)
		}
		if h.active[sid] == generation {
			delete(h.active, sid)
		}
		h.mu.Unlock()
	}()

	resp, err := caller.Prompt(ctx, &acp.PromptRequest{
		SessionID: sid,
		Prompt:    []acp.ContentBlock{acp.TextContentBlock(text)},
	})
	out, activity := col.result()
	h.mu.Lock()
	sameGeneration := h.generation == generation
	h.mu.Unlock()
	if !sameGeneration {
		return out, activity, fmt.Errorf("agent process changed during prompt")
	}
	if err != nil {
		return out, activity, fmt.Errorf("session/prompt: %w", err)
	}
	if resp.StopReason == acp.StopReasonCanceled {
		return out, activity, context.Canceled
	}
	if resp.StopReason != acp.StopReasonEndTurn {
		activity = append(activity, fmt.Sprintf("(stopReason: %s)", resp.StopReason))
	}
	return out, activity, nil
}

// Cancel asks the agent to stop the in-flight turn of one session.
func (h *Host) Cancel(ctx context.Context, sid acp.SessionID, generation uint64) error {
	h.mu.Lock()
	caller, alive := h.caller, h.alive && h.generation == generation
	h.mu.Unlock()
	if !alive || caller == nil {
		return nil
	}
	return caller.Cancel(ctx, &acp.CancelNotification{SessionID: sid})
}

func (h *Host) Abort(generation uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.generation == generation {
		h.shutdownLocked()
	}
}

func (h *Host) CloseSession(ctx context.Context, sid acp.SessionID) error {
	h.mu.Lock()
	if h.active[sid] != 0 {
		h.mu.Unlock()
		return ErrSessionBusy
	}
	if !h.sessions[sid] {
		h.mu.Unlock()
		return nil
	}
	caller, capabilities, alive := h.caller, h.capabilities, h.alive
	if !alive || caller == nil || capabilities == nil || capabilities.SessionCapabilities == nil || capabilities.SessionCapabilities.Close == nil {
		delete(h.sessions, sid)
		h.mu.Unlock()
		return nil
	}
	h.mu.Unlock()
	if _, err := caller.CloseSession(ctx, &acp.CloseSessionRequest{SessionID: sid}); err != nil {
		return fmt.Errorf("session/close: %w", err)
	}
	h.mu.Lock()
	delete(h.sessions, sid)
	h.mu.Unlock()
	return nil
}

func validateMCPServers(capabilities *acp.AgentCapabilities, servers []acp.MCPServer) error {
	for _, server := range servers {
		switch server.Type {
		case acp.MCPServerTypeHTTP:
			if capabilities == nil || capabilities.MCPCapabilities == nil || !capabilities.MCPCapabilities.HTTP {
				return fmt.Errorf("agent does not support HTTP MCP server %q", server.Name)
			}
		case acp.MCPServerTypeSSE:
			if capabilities == nil || capabilities.MCPCapabilities == nil || !capabilities.MCPCapabilities.SSE {
				return fmt.Errorf("agent does not support SSE MCP server %q", server.Name)
			}
		}
	}
	return nil
}

// Stop terminates the agent subprocess.
func (h *Host) Stop() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.shutdownLocked()
}

// shutdownLocked closes the connection and waits for the monitor goroutine to
// reap the process; requires h.mu to be held. cmd.Wait is never called here —
// the monitor goroutine is the sole waiter.
func (h *Host) shutdownLocked() {
	if !h.alive {
		return
	}
	h.alive = false
	if h.conn != nil {
		_ = h.conn.Close()
	}
	if h.stdin != nil {
		_ = h.stdin.Close()
	}
	if h.exited != nil {
		select {
		case <-h.exited:
		case <-time.After(5 * time.Second):
			if h.cmd != nil && h.cmd.Process != nil {
				_ = h.cmd.Process.Kill()
			}
			select {
			case <-h.exited:
			case <-time.After(5 * time.Second):
				log.Printf("acphost: process did not exit after kill")
			}
		}
	}
}
