// Package acphost runs one ACP agent subprocess (e.g. codex-acp) and exposes
// a session-oriented prompt API on top of the ACP client connection.
package acphost

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/view"
)

type Image struct {
	MIME string
	Data []byte
	URI  string
}

var ErrResumeUnsupported = errors.New("agent does not support session resume")
var ErrSessionBusy = errors.New("session already has a running turn")
var ErrClosed = errors.New("host is closed")
var ErrListUnsupported = errors.New("agent does not support session listing")
var ErrDeleteUnsupported = errors.New("agent does not support session deletion")

// ErrTurnCanceled marks a turn the agent ended itself with
// StopReasonCanceled (e.g. a permission request was rejected). The session
// stays consistent, so callers keep it instead of tearing the process down.
var ErrTurnCanceled = errors.New("agent canceled the turn")

// ErrStopUnconfirmed means the RPC was abandoned without an agent stop
// response. Its process may still be writing; callers must retain exclusion.
var ErrStopUnconfirmed = errors.New("agent stop was not confirmed")

// PromptSettled recognizes an explicit ACP response (including an error
// response), not transport EOF or a local cancellation of the pending RPC.
func PromptSettled(err error) bool {
	if errors.Is(err, ErrStopUnconfirmed) {
		return false
	}
	if err == nil || errors.Is(err, ErrTurnCanceled) {
		return true
	}
	var rpc *acp.Error
	var settled interface{ PromptSettled() bool }
	return errors.As(err, &rpc) || (errors.As(err, &settled) && settled.PromptSettled())
}

type settledPromptError struct{ cause error }

func (e settledPromptError) Error() string       { return e.cause.Error() }
func (e settledPromptError) Unwrap() error       { return e.cause }
func (e settledPromptError) PromptSettled() bool { return true }

func promptFailure(cause error, stopped bool) error {
	if PromptSettled(cause) || stopped {
		return settledPromptError{cause}
	}
	return fmt.Errorf("%w: %w", ErrStopUnconfirmed, cause)
}

// cancelNotifyTimeout bounds the session/cancel notification itself;
// cancelSettleTimeout bounds how long the agent gets to end the turn after
// being told to, before the call is abandoned and the session written off.
const (
	cancelNotifyTimeout = 5 * time.Second
	cancelSettleTimeout = 15 * time.Second
)

type Config struct {
	// Transport starts the agent process. Leaving it nil builds a local one
	// from the fields below, which is what a hub running an agent on its own
	// machine wants; a remote node supplies its own.
	Transport Transport

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
	proc         Process
	processes    map[uint64]Process
	conn         *acp.Conn
	caller       *acp.AgentCaller
	stdin        io.WriteCloser
	isClosed     bool
	alive        bool
	exited       chan struct{}
	collectors   map[acp.SessionID]*collector
	capabilities *acp.AgentCapabilities
	sessions     map[acp.SessionID]*sessionState
	opening      map[acp.SessionID]uint64
	active       map[acp.SessionID]uint64
	generation   uint64
	// adapter is the ACP agent's name and version as it introduced itself.
	adapter string
}

func New(cfg Config) *Host {
	if cfg.Permission == nil {
		cfg.Permission, _ = permission.New("deny")
	}
	if cfg.Transport == nil {
		cfg.Transport = LocalTransport{
			Command: cfg.Command, Args: cfg.Args, ProcessDir: cfg.ProcessDir, Env: cfg.Env,
		}
	}
	return &Host{
		cfg: cfg, collectors: map[acp.SessionID]*collector{}, sessions: map[acp.SessionID]*sessionState{},
		processes: map[uint64]Process{},
		opening:   map[acp.SessionID]uint64{}, active: map[acp.SessionID]uint64{},
	}
}

// collector accumulates streamed session updates for one in-flight prompt.
type collector struct {
	mu           sync.Mutex
	text         strings.Builder
	thought      string
	thoughtHead  string
	thoughtBytes int
	activity     []string
	tools        []view.Tool
	toolIndex    map[string]int
	timeline     []collectedSpan
	toolSpans    map[string]bool
	usage        view.Usage
	plan         []view.Step
	progress     func(view.Progress)
	settings     func() view.Settings
	generation   uint64
	overflow     bool
	ask          permission.AskFunc
	askUser      AskUserFunc
	ctx          context.Context
	cancelAsk    context.CancelFunc
}

// maxCollectBytes caps the aggregated assistant text so a runaway agent
// cannot balloon memory before the prompt timeout fires.
const maxCollectBytes = 1 << 20 // 1 MiB

// truncationMarker length is reserved inside maxCollectBytes so the final
// buffer never exceeds the cap.
const truncationMarker = "\n…(output truncated)"

func (c *collector) handle(u acp.SessionUpdate) {
	c.mu.Lock()
	switch u.SessionUpdate {
	case acp.SessionUpdateTypeAgentMessageChunk:
		if cb, ok := u.Content.(acp.ContentBlock); ok && cb.Type == acp.ContentBlockTypeText {
			c.writeText(cb.Text)
		}
	case acp.SessionUpdateTypeAgentThoughtChunk:
		if cb, ok := u.Content.(acp.ContentBlock); ok && cb.Type == acp.ContentBlockTypeText {
			c.writeThought(cb.Text)
		}
	case acp.SessionUpdateTypeToolCall, acp.SessionUpdateTypeToolCallUpdate:
		c.upsertTool(u)
	case acp.SessionUpdateTypeUsageUpdate:
		c.usage.ContextTokens = u.Used
		c.usage.ContextWindow = u.Size
		if u.Cost != nil {
			c.usage.Cost = &view.Cost{Amount: u.Cost.Amount, Currency: u.Cost.Currency}
		}
	case acp.SessionUpdateTypePlan:
		c.plan = planSteps(u.Entries)
	case acp.SessionUpdateTypeUserMessageChunk:
		// Only ever sent while replaying a loaded session, to help a client
		// rebuild a transcript it does not have. Feishu already holds ours,
		// and folding it into the answer would double the user's own words
		// back at them, so this is deliberately dropped.
		c.mu.Unlock()
		return
	case acp.SessionUpdateTypeConfigOptionUpdate,
		acp.SessionUpdateTypeCurrentModeUpdate,
		acp.SessionUpdateTypeAvailableCommandsUpdate:
		// applySettings already recorded these on the session; falling
		// through to the snapshot is what puts a mid-turn model or mode
		// switch on the card.
	case acp.SessionUpdateTypeSessionInfoUpdate:
		// Carries nothing but vendor _meta today (codex reports thread
		// status). Swallow it rather than logging it as unhandled.
		c.mu.Unlock()
		return
	default:
		c.mu.Unlock()
		noteUnhandled(u.SessionUpdate)
		return
	}
	p, fn := c.snapshot()
	c.mu.Unlock()
	if fn != nil {
		fn(p)
	}
}

// promptUsage publishes the counters that arrive only with the prompt response.
// Downstream ledgers consume progress, so retaining them without a final snapshot
// would still leave completed (and cancelled) attempts marked as unreported.
func (c *collector) promptUsage(usage *acp.Usage) {
	if usage == nil {
		return
	}
	c.mu.Lock()
	c.usage.Reported = true
	c.usage.TotalTokens = usage.TotalTokens
	c.usage.InputTokens = usage.InputTokens
	c.usage.OutputTokens = usage.OutputTokens
	if usage.ThoughtTokens != nil {
		c.usage.ThoughtTokens = *usage.ThoughtTokens
	}
	if usage.CachedReadTokens != nil {
		c.usage.CacheReadTokens = *usage.CachedReadTokens
	}
	if usage.CachedWriteTokens != nil {
		c.usage.CacheWriteTokens = *usage.CachedWriteTokens
	}
	p, fn := c.snapshot()
	c.mu.Unlock()
	if fn != nil {
		fn(p)
	}
}

// unhandledUpdates keeps the "we ignore this event" log to once per type per
// process, so an unmapped agent capability is visible without flooding logs.
var unhandledUpdates sync.Map

func noteUnhandled(kind acp.SessionUpdateType) {
	if _, seen := unhandledUpdates.LoadOrStore(kind, struct{}{}); !seen {
		log.Printf("acphost: ignoring session update %q", kind)
	}
}

func (c *collector) upsertTool(u acp.SessionUpdate) {
	id := string(u.ToolCallID)
	title := ""
	if u.Title != nil {
		title = *u.Title
	}
	if id == "" {
		id = title
	}
	if id == "" {
		return
	}
	if c.toolIndex == nil {
		c.toolIndex = map[string]int{}
	}
	kind := ""
	if u.Kind != nil {
		kind = string(*u.Kind)
	}
	status := toolStatus(u.Status, u.SessionUpdate == acp.SessionUpdateTypeToolCall)
	if i, ok := c.toolIndex[id]; ok {
		if title != "" {
			c.tools[i].Name = title
		}
		if kind != "" {
			c.tools[i].Kind = kind
		}
		if status != "" {
			c.tools[i].Status = status
		}
		applyToolIO(&c.tools[i], u)
	} else {
		if status == "" {
			status = view.ToolRunning
		}
		name := title
		if name == "" {
			name = id
		}
		tool := view.Tool{ID: id, Kind: kind, Name: name, Status: status}
		applyToolIO(&tool, u)
		c.toolIndex[id] = len(c.tools)
		c.tools = append(c.tools, tool)
		if title != "" {
			c.activity = append(c.activity, fmt.Sprintf("⚙ %s", title))
		}
	}
	// Status updates can arrive after later narration. Only the first
	// creation places a tool on the timeline; updates never move it.
	if u.SessionUpdate == acp.SessionUpdateTypeToolCall && !c.toolSpans[id] {
		if c.toolSpans == nil {
			c.toolSpans = map[string]bool{}
		}
		c.toolSpans[id] = true
		c.timeline = append(c.timeline, collectedSpan{Span: view.Span{Kind: "tool", Tool: id, At: time.Now().UTC()}})
	}
}

func applyToolIO(tool *view.Tool, u acp.SessionUpdate) {
	now := time.Now()
	if tool.StartedAt.IsZero() {
		tool.StartedAt = now
	}
	tool.UpdatedAt = now
	if s := formatAny(u.RawInput); s != "" {
		tool.Input = s
	}
	if s := formatAny(u.RawOutput); s != "" {
		tool.Output = s
	}
	// Agents that skip rawInput/rawOutput still describe the call through
	// content: a diff is what they are about to write, everything else is
	// what came back.
	diff, text := splitToolContent(u.Content)
	if tool.Input == "" && diff != "" {
		tool.Input = diff
	}
	if tool.Output == "" && text != "" {
		tool.Output = text
	}
	if tool.Detail == "" && u.Locations != nil {
		for _, loc := range *u.Locations {
			if loc.Path != "" {
				tool.Detail = loc.Path
				return
			}
		}
	}
}

func formatAny(v any) string {
	if v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case []byte:
		return strings.TrimSpace(string(t))
	default:
		raw, err := json.Marshal(t)
		if err != nil || string(raw) == "null" {
			return ""
		}
		return string(raw)
	}
}

// splitToolContent reads the tool-call content variants ACP defines: file
// diffs, plain content blocks and terminal handles.
func splitToolContent(v any) (diff, text string) {
	items, ok := v.([]acp.ToolCallContent)
	if !ok {
		if ptr, isPtr := v.(*[]acp.ToolCallContent); isPtr && ptr != nil {
			items = *ptr
		} else {
			return "", contentText(v)
		}
	}
	var diffs, texts []string
	for _, item := range items {
		switch item.Type {
		case acp.ToolCallContentTypeDiff:
			entry := item.NewText
			if item.Path != "" {
				entry = item.Path + "\n" + entry
			}
			diffs = append(diffs, strings.TrimSpace(entry))
		case acp.ToolCallContentTypeContent:
			if s := contentText(item.Content); s != "" {
				texts = append(texts, s)
			}
		}
	}
	return strings.Join(diffs, "\n"), strings.Join(texts, "\n")
}

func contentText(v any) string {
	switch t := v.(type) {
	case acp.ContentBlock:
		if t.Type == acp.ContentBlockTypeText {
			return strings.TrimSpace(t.Text)
		}
	case []acp.ContentBlock:
		var b strings.Builder
		for _, block := range t {
			if block.Type == acp.ContentBlockTypeText && block.Text != "" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(block.Text)
			}
		}
		return strings.TrimSpace(b.String())
	}
	return ""
}

func toolStatus(status *acp.ToolCallStatus, create bool) view.ToolStatus {
	if status == nil {
		if create {
			return view.ToolRunning
		}
		return ""
	}
	switch *status {
	case acp.ToolCallStatusCompleted:
		return view.ToolCompleted
	case acp.ToolCallStatusFailed:
		return view.ToolFailed
	default:
		return view.ToolRunning
	}
}

func (c *collector) snapshot() (view.Progress, func(view.Progress)) {
	p := view.Progress{
		Answer:    c.text.String(),
		Reasoning: c.reasoning(),
		Tools:     copyTools(c.tools),
		Usage:     c.usage,
		Plan:      append([]view.Step(nil), c.plan...),
		Timeline:  c.timelineSnapshot(),
	}
	if c.settings != nil {
		p.Settings = c.settings()
	}
	return p, c.progress
}

func copyTools(in []view.Tool) []view.Tool {
	if len(in) == 0 {
		return nil
	}
	out := make([]view.Tool, len(in))
	for i, tool := range in {
		tool.Children = copyTools(tool.Children)
		out[i] = tool
	}
	return out
}

// writeText appends a chunk, trimming at maxCollectBytes on a rune boundary
// and marking the result as truncated.
func (c *collector) writeText(chunk string) {
	if c.overflow || chunk == "" {
		return
	}
	used := c.text.Len()
	if used >= maxCollectBytes {
		c.overflow = true
		return
	}
	// Reserve room for the truncation marker so the final buffer stays
	// within maxCollectBytes.
	room := maxCollectBytes - used - len(truncationMarker)
	if room <= 0 {
		c.overflow = true
		return
	}
	if len(chunk) > room {
		body := chunk[:room]
		for len(body) > 0 && !utf8.ValidString(body) {
			body = body[:len(body)-1]
		}
		c.text.WriteString(body)
		c.text.WriteString(truncationMarker)
		c.appendTextSpan(body + truncationMarker)
		c.overflow = true
		return
	}
	c.text.WriteString(chunk)
	c.appendTextSpan(chunk)
}

const maxThoughtBytes = 64 << 10

func (c *collector) writeThought(chunk string) {
	if chunk == "" {
		return
	}
	c.appendThoughtSpan(len(chunk))
	c.thoughtBytes += len(chunk)
	c.thought += chunk
	if c.thoughtBytes <= maxThoughtBytes {
		return
	}
	// Preserve the opening context and keep moving the tail as chunks
	// arrive. Reserve space for the marker and never split a UTF-8 rune.
	if c.thoughtHead == "" {
		end := maxThoughtBytes / 2
		for end > 0 && !utf8.RuneStart(c.thought[end]) {
			end--
		}
		c.thoughtHead = strings.Clone(c.thought[:end])
	}
	start := len(c.thought) - (maxThoughtBytes - len(c.thoughtHead) - 64)
	if start <= 0 {
		return
	}
	for start < len(c.thought) && !utf8.RuneStart(c.thought[start]) {
		start++
	}
	c.thought = strings.Clone(c.thought[start:])
}

func (c *collector) reasoning() string {
	if c.thoughtHead == "" {
		return c.thought
	}
	skipped := c.thoughtBytes - len(c.thoughtHead) - len(c.thought)
	return fmt.Sprintf("%s\n[… 省略 %d 字节 …]\n%s", c.thoughtHead, skipped, c.thought)
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
	ch.h.applySettings(n.SessionID, n.Update)
	ch.h.mu.Lock()
	col := ch.h.collectors[n.SessionID]
	ch.h.mu.Unlock()
	if col != nil && col.generation == ch.generation {
		col.handle(n.Update)
	}
	return nil
}

func (ch *clientHandler) RequestPermission(ctx context.Context, req *acp.RequestPermissionRequest) (*acp.RequestPermissionResponse, error) {
	var kind acp.ToolKind
	if req.ToolCall.Kind != nil {
		kind = *req.ToolCall.Kind
	}
	title := ""
	if req.ToolCall.Title != nil {
		title = *req.ToolCall.Title
	}
	ch.h.mu.Lock()
	col := ch.h.collectors[req.SessionID]
	var ask permission.AskFunc
	if col != nil && col.generation == ch.generation {
		ask = col.ask
		if col.ctx != nil {
			ctx = col.ctx
		}
	}
	broker := ch.h.cfg.Permission
	ch.h.mu.Unlock()

	if broker.NeedsAsk(kind) && ask != nil {
		outcome, err := ask(ctx, permission.Ask{
			SessionID: string(req.SessionID), Generation: ch.generation, ToolCallID: string(req.ToolCall.ToolCallID),
			ToolName: title,
			Kind:     kind,
			Reason:   permissionReason(req.ToolCall),
			Options:  req.Options,
		})
		if err != nil {
			log.Printf("acphost: permission ask %q: %v", title, err)
			outcome = permission.Choose(false, req.Options)
		}
		log.Printf("acphost: permission request %q -> %s (asked)", title, outcome.Outcome)
		return &acp.RequestPermissionResponse{Outcome: outcome}, nil
	}

	outcome := broker.Decide(kind, req.Options)
	log.Printf("acphost: permission request %q -> %s", title, outcome.Outcome)
	return &acp.RequestPermissionResponse{Outcome: outcome}, nil
}

func permissionReason(call acp.ToolCallUpdate) string {
	if call.Locations == nil {
		return ""
	}
	parts := make([]string, 0, len(*call.Locations))
	for _, loc := range *call.Locations {
		if loc.Path != "" {
			parts = append(parts, loc.Path)
		}
	}
	return strings.Join(parts, ", ")
}

// ensureStarted launches the subprocess and performs ACP initialize if needed.
func (h *Host) ensureStarted(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.isClosed {
		return ErrClosed
	}
	if h.alive {
		return nil
	}
	proc, err := h.cfg.Transport.Start(ctx)
	if err != nil {
		return err
	}
	stdin := proc.Stdin()
	generation := h.generation + 1
	conn, err := acp.NewClient(proc.Stdout(), stdin, func(caller *acp.AgentCaller) acp.ClientHandler {
		h.caller = caller
		return &clientHandler{h: h, generation: generation}
	})
	if err != nil {
		proc.Kill()
		return fmt.Errorf("acp client: %w", err)
	}
	exited := make(chan struct{})
	h.proc = proc
	if h.processes == nil {
		h.processes = map[uint64]Process{}
	}
	h.processes[generation] = proc
	h.conn = conn
	h.stdin = stdin
	h.alive = true
	h.exited = exited
	h.generation = generation
	// Reset per-process state here as well as in the monitor goroutine: the
	// monitor skips its reset when a new process has already been started,
	// and stale P1-era session maps must never serve P2.
	h.collectors = map[acp.SessionID]*collector{}
	h.sessions = map[acp.SessionID]*sessionState{}
	h.opening = map[acp.SessionID]uint64{}
	h.active = map[acp.SessionID]uint64{}
	h.capabilities = nil

	// Sole waiter for this process; broadcasts exit before taking the lock
	// so shutdownLocked can wait on `exited` while holding h.mu.
	go func() {
		<-conn.Done()
		connErr := conn.Err()
		_ = proc.Wait()
		close(exited)
		h.mu.Lock()
		if evidence, ok := proc.(interface{ Stopped() bool }); ok && evidence.Stopped() {
			delete(h.processes, generation)
		}
		if h.proc == proc {
			h.alive = false
			h.collectors = map[acp.SessionID]*collector{}
			h.sessions = map[acp.SessionID]*sessionState{}
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
		ProtocolVersion: acp.ProtocolVersionV1,
		ClientInfo:      &acp.Implementation{Name: "steve", Version: "0.1.0"},
		// Advertise only what clientHandler really implements. An agent
		// that is not told the client can ask the user anything will never
		// try, so leaving this empty silently disabled every agent question.
		ClientCapabilities: &acp.ClientCapabilities{
			Elicitation: &acp.ElicitationCapabilities{Form: &acp.ElicitationFormCapabilities{}},
		},
	})
	if err != nil {
		h.shutdownLocked()
		return fmt.Errorf("initialize: %w", err)
	}
	name := "unknown"
	if resp.AgentInfo != nil {
		name = fmt.Sprintf("%s %s", resp.AgentInfo.Name, resp.AgentInfo.Version)
	}
	// The lock is already held here: this runs inside ensureStarted.
	h.capabilities = resp.AgentCapabilities
	h.adapter = name
	log.Printf("acphost: connected to agent %s (protocol v%d)", name, resp.ProtocolVersion)
	return nil
}

func (h *Host) OpenSession(ctx context.Context, sessionID acp.SessionID, cfg SessionConfig) (acp.SessionID, uint64, error) {
	if err := h.ensureStarted(ctx); err != nil {
		return "", 0, err
	}
	h.mu.Lock()
	generation := h.generation
	if h.sessions[sessionID] != nil {
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
		var state *sessionState
		switch {
		case capabilities != nil && capabilities.LoadSession:
			resp, err := caller.LoadSession(ctx, &request)
			if err != nil {
				return "", 0, fmt.Errorf("session/load: %w", err)
			}
			state = newSessionState(resp.Modes, resp.ConfigOptions)
			state.setMode(h.applyMode(ctx, caller, sessionID, resp.Modes))
		case capabilities != nil && capabilities.SessionCapabilities != nil && capabilities.SessionCapabilities.Resume != nil:
			resp, err := caller.ResumeSession(ctx, &acp.ResumeSessionRequest{
				SessionID: sessionID, Cwd: cfg.Workdir, MCPServers: cfg.MCPServers,
			})
			if err != nil {
				return "", 0, fmt.Errorf("session/resume: %w", err)
			}
			state = newSessionState(resp.Modes, resp.ConfigOptions)
			state.setMode(h.applyMode(ctx, caller, sessionID, resp.Modes))
		default:
			return "", 0, ErrResumeUnsupported
		}
		h.mu.Lock()
		if !h.alive || h.generation != generation {
			h.mu.Unlock()
			return "", 0, fmt.Errorf("agent process changed while opening session")
		}
		h.sessions[sessionID] = state
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
	state := newSessionState(resp.Modes, resp.ConfigOptions)
	state.setMode(h.applyMode(ctx, caller, resp.SessionID, resp.Modes))
	h.mu.Lock()
	if !h.alive || h.generation != generation {
		h.mu.Unlock()
		return "", 0, fmt.Errorf("agent process changed while opening session")
	}
	h.sessions[resp.SessionID] = state
	h.mu.Unlock()
	return resp.SessionID, generation, nil
}

// applyMode moves the session into the mode its permission policy implies.
// Agents default to approving their own writes, so without this the policy
// is never consulted. It returns the mode actually in force afterwards, so
// the caller can record what the session is really running under.
func (h *Host) applyMode(ctx context.Context, caller *acp.AgentCaller, sid acp.SessionID, modes *acp.SessionModeState) acp.SessionModeID {
	if modes == nil || caller == nil {
		return ""
	}
	ids := make([]string, 0, len(modes.AvailableModes))
	for _, mode := range modes.AvailableModes {
		ids = append(ids, string(mode.ID))
	}
	log.Printf("acphost: session modes current=%s available=%s", modes.CurrentModeID, strings.Join(ids, ","))
	wanted := h.cfg.Permission.SessionMode(ids)
	if wanted == "" || wanted == string(modes.CurrentModeID) {
		return modes.CurrentModeID
	}
	if _, err := caller.SetSessionMode(ctx, &acp.SetSessionModeRequest{
		SessionID: sid, ModeID: acp.SessionModeID(wanted),
	}); err != nil {
		log.Printf("acphost: set session mode %q: %v", wanted, err)
		return modes.CurrentModeID
	}
	log.Printf("acphost: session mode set to %q", wanted)
	return acp.SessionModeID(wanted)
}

// Prompt sends one user turn and blocks until the agent finishes it,
// returning the aggregated assistant text and tool-activity lines.
func (h *Host) Prompt(ctx context.Context, sid acp.SessionID, generation uint64, text string, progress func(view.Progress)) (string, []string, error) {
	return h.PromptTurn(ctx, sid, generation, text, nil, nil, nil, progress)
}

func (h *Host) PromptTurn(
	ctx context.Context,
	sid acp.SessionID,
	generation uint64,
	text string,
	images []Image,
	ask permission.AskFunc,
	askUser AskUserFunc,
	progress func(view.Progress),
) (string, []string, error) {
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
	caps := h.capabilities
	for _, media := range images {
		if len(media.Data) == 0 {
			continue
		}
		if caps == nil || caps.PromptCapabilities == nil || (media.URI == "" && !caps.PromptCapabilities.Image) || (media.URI != "" && !caps.PromptCapabilities.EmbeddedContext) {
			h.mu.Unlock()
			return "", nil, fmt.Errorf("agent does not support the attached media type %s", media.MIME)
		}
	}
	askCtx, cancelAsk := context.WithCancel(ctx)
	col := &collector{
		progress:   progress,
		settings:   h.sessionSettings(sid),
		generation: generation,
		ask:        ask,
		askUser:    askUser,
		ctx:        askCtx,
		cancelAsk:  cancelAsk,
	}
	h.collectors[sid] = col
	h.active[sid] = generation
	h.mu.Unlock()
	defer func() {
		cancelAsk()
		h.mu.Lock()
		if h.collectors[sid] == col {
			delete(h.collectors, sid)
		}
		if h.active[sid] == generation {
			delete(h.active, sid)
		}
		h.mu.Unlock()
	}()

	// A cancelled turn has to be cancelled *through* the agent rather than
	// by dropping the RPC. ACP ends a cancelled prompt by answering it with
	// StopReasonCanceled, and only an answered prompt leaves the session
	// consistent enough to keep using — which is the whole point when the
	// cancel came from the user sending a new instruction. Abandoning the
	// call instead leaves the agent working on a turn nobody is listening to
	// and the session too dirty to reuse.
	settled := make(chan struct{})
	var abandoned atomic.Bool
	defer close(settled)
	promptCtx, abandon := context.WithCancel(context.WithoutCancel(ctx))
	defer abandon()
	go func() {
		select {
		case <-settled:
			return
		case <-ctx.Done():
		}
		notifyCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), cancelNotifyTimeout)
		err := caller.Cancel(notifyCtx, &acp.CancelNotification{SessionID: sid})
		stop()
		if err != nil {
			log.Printf("acphost: cancel notify: %v", err)
		}
		select {
		case <-settled:
		case <-time.After(cancelSettleTimeout):
			// The agent did not end the turn. Give up on a clean stop; the
			// distinct error tells the caller it cannot prove writer quiescence.
			log.Printf("acphost: agent did not settle a cancelled turn in %s", cancelSettleTimeout)
			abandoned.Store(true)
			abandon()
		}
	}()

	resp, err := caller.Prompt(promptCtx, &acp.PromptRequest{
		SessionID: sid,
		Prompt:    promptBlocks(text, images, caps),
	})
	out, activity := col.result()
	h.mu.Lock()
	sameGeneration := h.generation == generation
	h.mu.Unlock()
	if !sameGeneration {
		cause := fmt.Errorf("agent process changed during prompt")
		if PromptSettled(err) {
			return out, activity, settledPromptError{cause}
		}
		return out, activity, promptFailure(cause, h.ProcessStopped(generation))
	}
	if err != nil {
		if abandoned.Load() {
			return out, activity, promptFailure(errors.Join(err, ctx.Err()), h.ProcessStopped(generation))
		}
		return out, activity, promptFailure(fmt.Errorf("session/prompt: %w", err), h.ProcessStopped(generation))
	}
	col.promptUsage(resp.Usage)
	if resp.StopReason == acp.StopReasonCanceled {
		return out, activity, fmt.Errorf("%w: %w", ErrTurnCanceled, context.Canceled)
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
	if col := h.collectors[sid]; alive && col != nil && col.generation == generation && col.cancelAsk != nil {
		col.cancelAsk()
	}
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

// ProcessStopped requires explicit termination evidence from the transport.
// Losing a stream or beginning a new generation is not such evidence.
func (h *Host) ProcessStopped(generation uint64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if generation == 0 || generation > h.generation {
		return false
	}
	proc, exists := h.processes[generation]
	if !exists {
		return true
	}
	if evidence, ok := proc.(interface{ Stopped() bool }); ok && evidence.Stopped() {
		delete(h.processes, generation)
		return true
	}
	return false
}

func (h *Host) CloseSession(ctx context.Context, sid acp.SessionID) error {
	h.mu.Lock()
	if h.active[sid] != 0 {
		h.mu.Unlock()
		return ErrSessionBusy
	}
	if h.sessions[sid] == nil {
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

func promptBlocks(text string, images []Image, caps *acp.AgentCapabilities) []acp.ContentBlock {
	if text == "" && len(images) > 0 {
		text = "[image]"
	}
	blocks := []acp.ContentBlock{acp.TextContentBlock(text)}
	if len(images) == 0 {
		return blocks
	}
	for _, img := range images {
		if len(img.Data) == 0 {
			continue
		}
		mime := img.MIME
		if mime == "" {
			mime = "image/png"
		}
		if img.URI != "" {
			if caps != nil && caps.PromptCapabilities != nil && caps.PromptCapabilities.EmbeddedContext {
				resource := acp.BlobEmbeddedResourceContents(base64.StdEncoding.EncodeToString(img.Data), img.URI)
				resource.MIMEType = &mime
				blocks = append(blocks, acp.ResourceContentBlock(resource))
			}
			continue
		}
		if caps == nil || caps.PromptCapabilities == nil || !caps.PromptCapabilities.Image {
			blocks[0] = acp.TextContentBlock(text + "\n\n[steve: images omitted; agent has no image prompt capability]")
			continue
		}
		blocks = append(blocks, acp.ImageContentBlock(base64.StdEncoding.EncodeToString(img.Data), mime))
	}
	return blocks
}

// SupportsHTTPMCP reports whether the agent advertises the HTTP MCP
// transport, starting the process if needed: whether to inject an HTTP
// server has to be decided before the session's capabilities are assembled.
func (h *Host) SupportsHTTPMCP(ctx context.Context) (bool, error) {
	if err := h.ensureStarted(ctx); err != nil {
		return false, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	caps := h.capabilities
	return caps != nil && caps.MCPCapabilities != nil && caps.MCPCapabilities.HTTP, nil
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

// Stop terminates the agent subprocess. The next OpenSession or Prompt
// starts a new process (crash recovery / lazy restart).
func (h *Host) Stop() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.shutdownLocked()
}

// Close permanently stops the host so shutdown cannot spawn a replacement
// process. Manager.Stop uses this; Abort still uses Stop so a live manager
// can restart after a stuck turn.
func (h *Host) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.isClosed = true
	h.shutdownLocked()
}

// AllProcessesStopped requires positive transport evidence, including older
// generations whose connection has already closed.
func (h *Host) AllProcessesStopped() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for generation, proc := range h.processes {
		proof, ok := proc.(interface{ Stopped() bool })
		if !ok || !proof.Stopped() {
			return false
		}
		delete(h.processes, generation)
	}
	return true
}

// shutdownLocked closes the connection and waits for the monitor goroutine to
// reap the process; requires h.mu to be held. It releases h.mu while waiting
// so in-flight session notifications can drain instead of blocking on the
// lock (conn.Done waits for the notification loop, whose handler takes h.mu).
// cmd.Wait is never called here — the monitor goroutine is the sole waiter.
func (h *Host) shutdownLocked() {
	if !h.alive {
		return
	}
	h.alive = false
	conn, stdin, exited, proc := h.conn, h.stdin, h.exited, h.proc
	if conn != nil {
		_ = conn.Close()
	}
	if stdin != nil {
		_ = stdin.Close()
	}
	if exited == nil {
		return
	}
	h.mu.Unlock()
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		proc.Kill()
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			log.Printf("acphost: process did not exit after kill")
		}
	}
	h.mu.Lock()
}

// applySettings files a session-scoped notification against the session it
// belongs to. These outlive the turn that happens to be running — an agent
// may switch model between turns — so they are recorded on the session
// rather than on the collector.
func (h *Host) applySettings(sid acp.SessionID, u acp.SessionUpdate) {
	h.mu.Lock()
	state := h.sessions[sid]
	h.mu.Unlock()
	if state == nil {
		return
	}
	switch u.SessionUpdate {
	case acp.SessionUpdateTypeConfigOptionUpdate:
		state.setOptions(u.ConfigOptions)
	case acp.SessionUpdateTypeCurrentModeUpdate:
		state.setMode(u.CurrentModeID)
	case acp.SessionUpdateTypeAvailableCommandsUpdate:
		state.setCommands(u.AvailableCommands)
	}
}

// sessionSettings returns a reader for the session's current settings. It is
// resolved lazily so a snapshot taken late in a turn sees a model the agent
// switched to mid-turn.
func (h *Host) sessionSettings(sid acp.SessionID) func() view.Settings {
	return func() view.Settings {
		h.mu.Lock()
		state := h.sessions[sid]
		h.mu.Unlock()
		return state.settings()
	}
}

// Settings reports what the agent last said about how this session is
// configured. It is empty for a session this host does not know.
func (h *Host) Settings(sid acp.SessionID) view.Settings {
	h.mu.Lock()
	state := h.sessions[sid]
	adapter := h.adapter
	h.mu.Unlock()
	out := state.settings()
	out.Adapter = adapter
	out.Options = optionsView(h.Options(sid))
	return out
}
