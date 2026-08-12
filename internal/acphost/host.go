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
)

type Config struct {
	Command    string
	Args       []string
	Workdir    string
	Env        []string
	Permission string // auto | always_allow | deny
}

// Host owns the agent subprocess and its ACP connection. It restarts the
// process lazily on the next call after a crash.
type Host struct {
	cfg Config

	mu         sync.Mutex
	cmd        *exec.Cmd
	conn       *acp.Conn
	caller     *acp.AgentCaller
	stdin      io.WriteCloser
	alive      bool
	exited     chan struct{}
	collectors map[acp.SessionID]*collector
}

func New(cfg Config) *Host {
	return &Host{cfg: cfg, collectors: map[acp.SessionID]*collector{}}
}

// collector accumulates streamed session updates for one in-flight prompt.
type collector struct {
	mu       sync.Mutex
	text     strings.Builder
	activity []string
	progress func(string)
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
type clientHandler struct{ h *Host }

func (ch *clientHandler) Update(_ context.Context, n *acp.SessionNotification) error {
	ch.h.mu.Lock()
	col := ch.h.collectors[n.SessionID]
	ch.h.mu.Unlock()
	if col != nil {
		col.handle(n.Update)
	}
	return nil
}

func (ch *clientHandler) RequestPermission(_ context.Context, req *acp.RequestPermissionRequest) (*acp.RequestPermissionResponse, error) {
	outcome := decidePermission(ch.h.cfg.Permission, req.Options)
	title := ""
	if req.ToolCall.Title != nil {
		title = *req.ToolCall.Title
	}
	log.Printf("acphost: permission request %q -> %s", title, outcome.Outcome)
	return &acp.RequestPermissionResponse{Outcome: outcome}, nil
}

func decidePermission(policy string, options []acp.PermissionOption) acp.RequestPermissionOutcome {
	pick := func(kinds ...acp.PermissionOptionKind) *acp.PermissionOption {
		for _, k := range kinds {
			for i := range options {
				if options[i].Kind == k {
					return &options[i]
				}
			}
		}
		return nil
	}
	var opt *acp.PermissionOption
	switch policy {
	case "deny":
		opt = pick(acp.PermissionOptionKindRejectOnce, acp.PermissionOptionKindRejectAlways)
	case "always_allow":
		opt = pick(acp.PermissionOptionKindAllowAlways, acp.PermissionOptionKindAllowOnce)
	default: // auto
		opt = pick(acp.PermissionOptionKindAllowOnce, acp.PermissionOptionKindAllowAlways)
	}
	if opt == nil {
		return acp.CanceledRequestPermissionOutcome()
	}
	return acp.SelectedRequestPermissionOutcome(opt.OptionID)
}

// ensureStarted launches the subprocess and performs ACP initialize if needed.
func (h *Host) ensureStarted(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.alive {
		return nil
	}
	if err := os.MkdirAll(h.cfg.Workdir, 0o755); err != nil {
		return fmt.Errorf("create agent workdir: %w", err)
	}
	cmd := exec.Command(h.cfg.Command, h.cfg.Args...)
	cmd.Dir = h.cfg.Workdir
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
	conn, err := acp.NewClient(stdout, stdin, func(caller *acp.AgentCaller) acp.ClientHandler {
		h.caller = caller
		return &clientHandler{h: h}
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
	log.Printf("acphost: connected to agent %s (protocol v%d)", name, resp.ProtocolVersion)
	return nil
}

// NewChatSession creates a fresh ACP session rooted at the configured workdir.
func (h *Host) NewChatSession(ctx context.Context) (acp.SessionID, error) {
	if err := h.ensureStarted(ctx); err != nil {
		return "", err
	}
	resp, err := h.caller.NewSession(ctx, &acp.NewSessionRequest{
		Cwd:        h.cfg.Workdir,
		MCPServers: []acp.MCPServer{},
	})
	if err != nil {
		return "", fmt.Errorf("session/new: %w", err)
	}
	return resp.SessionID, nil
}

// Prompt sends one user turn and blocks until the agent finishes it,
// returning the aggregated assistant text and tool-activity lines.
func (h *Host) Prompt(ctx context.Context, sid acp.SessionID, text string, progress func(string)) (string, []string, error) {
	if err := h.ensureStarted(ctx); err != nil {
		return "", nil, err
	}
	col := &collector{progress: progress}
	h.mu.Lock()
	h.collectors[sid] = col
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.collectors, sid)
		h.mu.Unlock()
	}()

	resp, err := h.caller.Prompt(ctx, &acp.PromptRequest{
		SessionID: sid,
		Prompt:    []acp.ContentBlock{acp.TextContentBlock(text)},
	})
	out, activity := col.result()
	if err != nil {
		return out, activity, fmt.Errorf("session/prompt: %w", err)
	}
	if resp.StopReason != acp.StopReasonEndTurn {
		activity = append(activity, fmt.Sprintf("(stopReason: %s)", resp.StopReason))
	}
	return out, activity, nil
}

// Cancel asks the agent to stop the in-flight turn of one session.
func (h *Host) Cancel(ctx context.Context, sid acp.SessionID) error {
	h.mu.Lock()
	caller, alive := h.caller, h.alive
	h.mu.Unlock()
	if !alive || caller == nil {
		return nil
	}
	return caller.Cancel(ctx, &acp.CancelNotification{SessionID: sid})
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
			<-h.exited
		}
	}
}
