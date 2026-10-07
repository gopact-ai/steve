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
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/gopact-ai/acp"
	"github.com/gopact-ai/steve/internal/permission"
	"github.com/gopact-ai/steve/internal/procgroup"
	"github.com/gopact-ai/steve/internal/view"
)

type Image struct {
	MIME string
	Data []byte
	URI  string
}

var ErrResumeUnsupported = errors.New("agent does not support session resume")
var ErrLoadUnsupported = errors.New("agent does not support session history replay")
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
	if errors.Is(err, ErrStopUnconfirmed) || errors.Is(err, ErrSessionOperationUnconfirmed) {
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
	// Started learns the identity of each local agent process group; see
	// LocalTransport.Started.
	Started func(procgroup.Identity)
	// NoRestart binds this host to its original native process. Recovery must
	// create a new, explicitly admitted execution rather than reuse this host.
	NoRestart bool
}

type SessionConfig struct {
	Workdir    string
	MCPServers []acp.MCPServer
	// ReplayHistory requests session/load and its history notifications.
	// Without a history consumer it does not rebuild a transcript.
	// Otherwise an existing session prefers resume, which does not replay it.
	ReplayHistory bool
}

// Host owns the agent subprocess and its ACP connection. It restarts the
// process lazily on the next call after a crash.
type Host struct {
	cfg Config

	mu        sync.Mutex
	proc      Process
	processes map[uint64]Process
	conn      *acp.Conn
	caller    *acp.AgentCaller
	stdin     io.WriteCloser
	isClosed  bool
	alive     bool
	exited    chan struct{}
	// settling holds, for each process whose Wait has not returned, a
	// channel closed once it has: what the agent left in its process group
	// is settled, whether its stop is confirmed or not.
	settling     map[uint64]chan struct{}
	collectors   map[acp.SessionID]*collector
	capabilities *acp.AgentCapabilities
	sessions     map[acp.SessionID]*sessionState
	opening      map[acp.SessionID]uint64
	active       map[acp.SessionID]uint64
	// Lifecycle outcomes survive process/session map resets until the original
	// request settles or its exact owned process has confirmed stop evidence.
	newOpening        *newOpening
	sessionOperations map[acp.SessionID]*sessionOperation
	generation        uint64
	// adapter is the ACP agent's name and version as it introduced itself.
	adapter string
}

func New(cfg Config) *Host {
	if cfg.Permission == nil {
		cfg.Permission, _ = permission.New("deny")
	}
	if cfg.Transport == nil {
		cfg.Transport = LocalTransport{
			Command: cfg.Command, Args: cfg.Args, ProcessDir: cfg.ProcessDir, Env: cfg.Env, Started: cfg.Started,
		}
	}
	return &Host{
		cfg: cfg, collectors: map[acp.SessionID]*collector{}, sessions: map[acp.SessionID]*sessionState{},
		processes: map[uint64]Process{}, settling: map[uint64]chan struct{}{},
		opening: map[acp.SessionID]uint64{}, active: map[acp.SessionID]uint64{},
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
		slog.Warn(fmt.Sprintf("acphost: ignoring session update %q", kind))
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

// omitted marks where an overflowing thought lost its middle, and how
// many bytes. It carries no words: the collector runs wherever the agent
// does, and the marker reaches readers of every language unchanged.
const omitted = "\n[… %d B …]\n"

func (c *collector) reasoning() string {
	if c.thoughtHead == "" {
		return c.thought
	}
	skipped := c.thoughtBytes - len(c.thoughtHead) - len(c.thought)
	return c.thoughtHead + fmt.Sprintf(omitted, skipped) + c.thought
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

func (ch *clientHandler) Update(ctx context.Context, n *acp.SessionNotification) error {
	sequence, ok := acp.NotificationSequence(ctx)
	if !ok {
		return fmt.Errorf("session/update: missing inbound notification sequence")
	}
	ch.h.mu.Lock()
	if ch.h.generation != ch.generation {
		ch.h.mu.Unlock()
		return nil
	}
	if op := ch.h.sessionOperations[n.SessionID]; op != nil {
		if op.method == "new" && op.generation == ch.generation && op.state != nil {
			err := ch.h.stageBoundNewSettingsLocked(n.SessionID, op, n.Update, sequence)
			ch.h.mu.Unlock()
			return err
		}
		if op.generation == ch.generation && op.state != nil {
			applySessionSettingsAt(op.state, n.Update, sequence)
			if op.method == "configure" && op.original != nil && ch.h.sessions[n.SessionID] == op.original {
				// An independent configuration notification is already an Actual
				// observation for the still-running original prompt/context.
				applySessionSettingsAt(op.original, n.Update, sequence)
			}
		}
		// Config can run mid-turn. Continue delivering the original prompt's
		// output/callbacks while keeping lifecycle restore history isolated.
		col := ch.h.collectors[n.SessionID]
		forward := op.method == "configure" && op.generation == ch.generation && col != nil && col.generation == ch.generation
		ch.h.mu.Unlock()
		if forward {
			col.handle(n.Update)
		}
		return nil
	}
	if ch.h.sessions[n.SessionID] == nil {
		err := ch.h.stageNewSettingsLocked(n.SessionID, n.Update, sequence)
		ch.h.mu.Unlock()
		return err
	}
	applySessionSettingsAt(ch.h.sessions[n.SessionID], n.Update, sequence)
	col := ch.h.collectors[n.SessionID]
	ch.h.mu.Unlock()
	if col != nil && col.generation == ch.generation {
		col.handle(n.Update)
	}
	return nil
}

func (ch *clientHandler) RequestPermission(ctx context.Context, req *acp.RequestPermissionRequest) (*acp.RequestPermissionResponse, error) {
	// ACP makes ToolCall.Kind optional; the internal permission question is
	// persisted and sent across nodes with a concrete, valid discriminator.
	kind := acp.ToolKindOther
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
			slog.Error(fmt.Sprintf("acphost: permission ask %q: %v", title, err))
			outcome = permission.Choose(false, req.Options)
		}
		slog.Info(fmt.Sprintf("acphost: permission request %q -> %s (asked)", title, outcome.Outcome))
		return &acp.RequestPermissionResponse{Outcome: outcome}, nil
	}

	outcome := broker.Decide(kind, req.Options)
	slog.Info(fmt.Sprintf("acphost: permission request %q -> %s", title, outcome.Outcome))
	return &acp.RequestPermissionResponse{Outcome: outcome}, nil
}

// permissionReasonLimit bounds the Markdown a permission question carries;
// the full tool input still reaches the transcript through the tool record.
const permissionReasonLimit = 4096

// permissionReason turns a tool call into the Markdown a person reads
// before approving it: the command and where it runs, the files it edits,
// the agent's stated purpose. Inputs no field name explains are shown as
// JSON rather than hidden, since approving blind is worse than reading JSON.
func permissionReason(call acp.ToolCallUpdate) string {
	var lines []string
	files := map[string]bool{}
	addFile := func(path string) {
		if path = strings.TrimSpace(path); path != "" && !files[path] {
			files[path] = true
			lines = append(lines, "- `"+path+"`")
		}
	}
	if input, ok := call.RawInput.(map[string]any); ok && len(input) > 0 {
		rest := map[string]any{}
		for key, value := range input {
			rest[key] = value
		}
		if command := commandText(input, rest); command != "" {
			lines = append(lines, "```sh\n"+command+"\n```")
		}
		for _, key := range []string{"cwd", "workdir", "working_directory"} {
			if dir, ok := rest[key].(string); ok && strings.TrimSpace(dir) != "" {
				lines = append(lines, "cwd: `"+strings.TrimSpace(dir)+"`")
				delete(rest, key)
				break
			}
		}
		for _, key := range []string{"description", "reason", "justification", "purpose"} {
			if text, ok := rest[key].(string); ok && strings.TrimSpace(text) != "" {
				lines = append(lines, strings.TrimSpace(text))
				delete(rest, key)
			}
		}
		for _, key := range []string{"file_path", "filePath", "path", "notebook_path", "target_file"} {
			if path, ok := rest[key].(string); ok {
				addFile(path)
				delete(rest, key)
			}
		}
		if paths, ok := rest["paths"].([]any); ok {
			for _, path := range paths {
				if text, ok := path.(string); ok {
					addFile(text)
				}
			}
			delete(rest, "paths")
		}
		// File contents and edits are too large to read here and appear as
		// diffs in the transcript; the remaining fields are what the agent
		// asked for and are shown as they are.
		for _, key := range []string{"content", "old_string", "new_string", "edits", "old_str", "new_str", "contents", "text"} {
			delete(rest, key)
		}
		if len(lines) == 0 && len(rest) > 0 {
			if raw, err := json.MarshalIndent(rest, "", "  "); err == nil {
				lines = append(lines, "```json\n"+string(raw)+"\n```")
			}
		}
	} else if text := formatAny(call.RawInput); text != "" {
		lines = append(lines, "```\n"+text+"\n```")
	}
	if call.Content != nil {
		for _, item := range *call.Content {
			switch item.Type {
			case acp.ToolCallContentTypeDiff:
				addFile(item.Path)
			case acp.ToolCallContentTypeContent:
				if item.Content.Type == acp.ContentBlockTypeText && strings.TrimSpace(item.Content.Text) != "" {
					lines = append(lines, strings.TrimSpace(item.Content.Text))
				}
			}
		}
	}
	if call.Locations != nil {
		for _, loc := range *call.Locations {
			addFile(loc.Path)
		}
	}
	reason := strings.Join(lines, "\n\n")
	if len(reason) > permissionReasonLimit {
		cut := permissionReasonLimit
		for cut > 0 && !utf8.RuneStart(reason[cut]) {
			cut--
		}
		reason = reason[:cut] + "…"
		// A truncated fence would swallow the rest of the page.
		if strings.Count(reason, "```")%2 == 1 {
			reason += "\n```"
		}
	}
	return reason
}

// commandText renders the command a tool call wants to run, whether the
// agent sent it as one string or as argv, and removes it from rest.
func commandText(input, rest map[string]any) string {
	for _, key := range []string{"command", "cmd", "commandLine", "script"} {
		value, ok := input[key]
		if !ok {
			continue
		}
		delete(rest, key)
		switch v := value.(type) {
		case string:
			return strings.TrimSpace(v)
		case []any:
			parts := make([]string, 0, len(v))
			for _, part := range v {
				text := fmt.Sprint(part)
				if strings.ContainsAny(text, " \t\"'$`\\") {
					text = strconv.Quote(text)
				}
				parts = append(parts, text)
			}
			return strings.Join(parts, " ")
		}
	}
	return ""
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
	if h.cfg.NoRestart && h.generation != 0 {
		return ErrClosed
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
	exited, settled := make(chan struct{}), make(chan struct{})
	h.proc = proc
	if h.processes == nil {
		h.processes = map[uint64]Process{}
	}
	if h.settling == nil {
		h.settling = map[uint64]chan struct{}{}
	}
	h.processes[generation] = proc
	h.settling[generation] = settled
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

	go h.watch(generation, proc, conn, exited, settled)
	if local, ok := proc.(*localProcess); ok && local.unidentified != nil {
		// No record can name the group, so nothing could confirm its stop
		// once this process is gone. The agent is ended before it serves
		// anything, and stays on the books until its stop is confirmed.
		proc.Kill()
		h.shutdownLocked()
		return fmt.Errorf("identify agent process group: %w", local.unidentified)
	}

	initCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	resp, err := h.caller.Initialize(initCtx, &acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionV1,
		ClientInfo:      &acp.Implementation{Name: "steve", Version: "0.1.0"},
		// Advertise only what clientHandler really implements. An agent
		// that is not told the client can ask the user anything will never
		// try, so leaving this empty silently disabled every agent question.
		ClientCapabilities: &acp.ClientCapabilities{
			Session:     &acp.ClientSessionCapabilities{ConfigOptions: &acp.SessionConfigOptionsCapabilities{Boolean: &acp.BooleanConfigOptionCapabilities{}}},
			Elicitation: &acp.ElicitationCapabilities{Form: &acp.ElicitationFormCapabilities{}},
		},
	})
	if err != nil {
		h.shutdownLocked()
		return fmt.Errorf("initialize: %w", err)
	}
	if resp.ProtocolVersion != acp.ProtocolVersionV1 {
		proc.Kill()
		h.shutdownLocked()
		return fmt.Errorf("initialize: unsupported protocol version %d (client supports %d)", resp.ProtocolVersion, acp.ProtocolVersionV1)
	}
	name := "unknown"
	if resp.AgentInfo != nil {
		name = fmt.Sprintf("%s %s", resp.AgentInfo.Name, resp.AgentInfo.Version)
	}
	// The lock is already held here: this runs inside ensureStarted.
	h.capabilities = resp.AgentCapabilities
	h.adapter = name
	slog.Info(fmt.Sprintf("acphost: connected to agent %s (protocol v%d)", name, resp.ProtocolVersion))
	return nil
}

// watch is the sole waiter for one agent process. Once the agent has exited
// the host is free to start another, even while Wait still settles what
// the exited one left in its process group; the process stays on the books
// until that stop is confirmed. Exit is broadcast before taking the lock so
// shutdownLocked can wait on `exited` without holding h.mu. Close waits
// for Wait only a bounded time: while a member of the group runs, this
// goroutine and the one in Wait outlive the host's Close, and end once the
// group has.
func (h *Host) watch(generation uint64, proc Process, conn *acp.Conn, exited, settled chan struct{}) {
	endConnection(conn, proc)
	connErr := conn.Err()
	waited := make(chan struct{})
	go func() {
		// Wait releases the transport; how the agent ended is read from
		// the connection error, and Stopped carries the evidence.
		_ = proc.Wait()
		close(waited)
	}()
	select {
	case <-proc.Exited():
	case <-waited:
	}
	close(exited)
	h.mu.Lock()
	if h.proc == proc {
		if opening := h.newOpening; opening != nil && opening.process == proc && opening.generation == generation {
			closeNewScratch(opening)
		}
		h.alive = false
		h.collectors = map[acp.SessionID]*collector{}
		h.sessions = map[acp.SessionID]*sessionState{}
		h.opening = map[acp.SessionID]uint64{}
		h.active = map[acp.SessionID]uint64{}
		h.capabilities = nil
	}
	h.mu.Unlock()
	if connErr != nil && !errors.Is(connErr, io.EOF) {
		slog.Error(fmt.Sprintf("acphost: connection closed: %v", connErr))
	} else {
		slog.Info("acphost: agent process exited")
	}
	<-waited
	h.mu.Lock()
	if proc.Stopped() {
		delete(h.processes, generation)
	}
	delete(h.settling, generation)
	h.mu.Unlock()
	close(settled)
}

// ExitedOutputWait bounds how long an agent's output is read once the agent
// has exited. The transport ends what the agent left in its process group
// as it exits, which closes the output they held within milliseconds;
// output still open then is held by something the agent left outside its
// group, which need never close it.
const ExitedOutputWait = 2 * time.Second

// endConnection returns once the connection to the agent proc runs has
// ended: the agent closed its output, or the agent exited and what it
// wrote before was read, the connection closed if its output stays open
// ExitedOutputWait after.
func endConnection(conn *acp.Conn, proc Process) {
	select {
	case <-conn.Done():
		return
	case <-proc.Exited():
	}
	held := time.NewTimer(ExitedOutputWait)
	defer held.Stop()
	select {
	case <-conn.Done():
	case <-held.C:
		slog.Warn(fmt.Sprintf("acphost: the agent exited and its output is still open after %s; the connection is closed", ExitedOutputWait))
		_ = conn.Close()
		<-conn.Done()
	}
}

func (h *Host) OpenSession(ctx context.Context, sessionID acp.SessionID, cfg SessionConfig) (acp.SessionID, uint64, error) {
	if cfg.ReplayHistory && sessionID == "" {
		return "", 0, fmt.Errorf("history replay requires an existing session id")
	}
	h.mu.Lock()
	if sessionID == "" {
		if err := h.newOpeningBlockedLocked(); err != nil {
			h.mu.Unlock()
			return "", 0, err
		}
	}
	blocked := h.SessionBlockedLocked(sessionID)
	h.mu.Unlock()
	if blocked != nil {
		return "", 0, blocked
	}
	if err := h.ensureStarted(ctx); err != nil {
		return "", 0, err
	}
	h.mu.Lock()
	if err := h.SessionBlockedLocked(sessionID); err != nil {
		h.mu.Unlock()
		return "", 0, err
	}
	generation := h.generation
	if sessionID != "" && h.opening[sessionID] != 0 {
		h.mu.Unlock()
		return "", 0, fmt.Errorf("session %q is already being opened", sessionID)
	}
	if h.sessions[sessionID] != nil && !cfg.ReplayHistory {
		h.mu.Unlock()
		return sessionID, generation, nil
	}
	caller, capabilities := h.caller, h.capabilities
	if err := validateMCPServers(capabilities, cfg.MCPServers); err != nil {
		h.mu.Unlock()
		return "", 0, err
	}
	if sessionID != "" {
		method := ""
		switch {
		case !cfg.ReplayHistory && capabilities != nil && capabilities.SessionCapabilities != nil && capabilities.SessionCapabilities.Resume != nil:
			method = "resume"
		case capabilities != nil && capabilities.LoadSession:
			method = "load"
		case cfg.ReplayHistory:
			h.mu.Unlock()
			return "", 0, ErrLoadUnsupported
		default:
			h.mu.Unlock()
			return "", 0, ErrResumeUnsupported
		}
		op, err := h.beginSessionOperationLocked(ctx, sessionID, method)
		h.mu.Unlock()
		if err != nil {
			return "", 0, err
		}
		var modes *acp.SessionModeState
		var options *[]acp.SessionConfigOption
		var receipt acp.ResponseReceipt
		if method == "load" {
			var resp *acp.LoadSessionResponse
			resp, receipt, err = caller.LoadSessionWithReceipt(ctx, &acp.LoadSessionRequest{SessionID: sessionID, Cwd: cfg.Workdir, MCPServers: cfg.MCPServers})
			if err == nil {
				modes, options = resp.Modes, resp.ConfigOptions
			}
		} else {
			var resp *acp.ResumeSessionResponse
			resp, receipt, err = caller.ResumeSessionWithReceipt(ctx, &acp.ResumeSessionRequest{SessionID: sessionID, Cwd: cfg.Workdir, MCPServers: cfg.MCPServers})
			if err == nil {
				modes, options = resp.Modes, resp.ConfigOptions
			}
		}
		if err == nil && receipt.Sequence == 0 {
			err = fmt.Errorf("session/%s: successful result lacks inbound receipt", method)
		}
		if err == nil {
			h.mu.Lock()
			applyOpenResponseAt(op.state, modes, options, receipt.Sequence)
			h.mu.Unlock()
			h.applyMode(ctx, caller, sessionID, generation, op.state, modes)
		}
		h.mu.Lock()
		err = h.finishSessionOperationLocked(ctx, sessionID, op, err)
		h.mu.Unlock()
		if err != nil {
			return "", 0, err
		}
		return sessionID, generation, nil
	}
	opening, err := h.beginNewOpeningLocked(ctx)
	h.mu.Unlock()
	if err != nil {
		return "", 0, err
	}
	resp, receipt, err := caller.NewSessionWithReceipt(ctx, &acp.NewSessionRequest{Cwd: cfg.Workdir, MCPServers: cfg.MCPServers})
	h.mu.Lock()
	if err != nil {
		err = h.failNewOpeningLocked(ctx, opening, err, "", false)
		h.mu.Unlock()
		return "", 0, err
	}
	op, err := h.bindNewOpeningLocked(ctx, opening, resp, receipt)
	h.mu.Unlock()
	if err != nil {
		return "", 0, err
	}
	h.applyMode(ctx, caller, resp.SessionID, generation, op.state, resp.Modes)
	h.mu.Lock()
	if opening.cause == nil {
		if size, sizeErr := scratchStateBytes(op.state); sizeErr != nil || size > maxNewScratchPerSID {
			opening.cause = errNewScratchLimit
		}
	}
	if opening.cause != nil {
		err = h.finishSessionOperationLocked(ctx, resp.SessionID, op, opening.cause)
		opening.pending = false
		opening.cause = err
	} else {
		err = h.finishSessionOperationLocked(ctx, resp.SessionID, op, nil)
		if h.newOpening == opening {
			h.newOpening = nil
		}
	}
	h.mu.Unlock()
	if err != nil {
		return "", 0, err
	}
	return resp.SessionID, generation, nil
}

// applyMode moves the session into the mode its permission policy implies.
// Agents default to approving their own writes, so without this the policy
// is never consulted. The selector policy is unchanged; a matched successful
// standard set_mode response confirms its available target at the real receipt
// sequence, while later independent mode notifications remain authoritative.
func (h *Host) applyMode(ctx context.Context, caller *acp.AgentCaller, sid acp.SessionID, generation uint64, state *sessionState, modes *acp.SessionModeState) {
	if modes == nil || caller == nil {
		return
	}
	ids := make([]string, 0, len(modes.AvailableModes))
	for _, mode := range modes.AvailableModes {
		ids = append(ids, string(mode.ID))
	}
	slog.Info(fmt.Sprintf("acphost: session modes current=%s available=%s", modes.CurrentModeID, strings.Join(ids, ",")))
	wanted := h.cfg.Permission.SessionMode(ids)
	if wanted == "" || wanted == string(modes.CurrentModeID) {
		return
	}
	_, receipt, err := caller.SetSessionModeWithReceipt(ctx, &acp.SetSessionModeRequest{SessionID: sid, ModeID: acp.SessionModeID(wanted)})
	if err != nil {
		slog.Error(fmt.Sprintf("acphost: set session mode %q: %v", wanted, err))
		return
	}
	if receipt.Sequence == 0 {
		slog.Error("acphost: set session mode succeeded without inbound receipt")
		return
	}
	// Consume only a successful matched set_mode response; receipt metadata
	// orders frames but is not an Actual value or execution/cleanup proof.
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.alive || h.generation != generation || h.caller != caller {
		return
	}
	valid := h.sessions[sid] == state
	if op := h.sessionOperations[sid]; op != nil {
		valid = op.generation == generation && op.state == state
	}
	if !valid {
		return
	}
	// This is method-specific success confirmation, not optimistic request
	// echo or arbitrary nonzero-receipt success. The typed standard {} ACK
	// confirms the validated available target; a higher mode clock still wins.
	state.setModeAt(acp.SessionModeID(wanted), receipt.Sequence)
	slog.Info(fmt.Sprintf("acphost: session mode %q confirmed at inbound sequence %d", wanted, receipt.Sequence))
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
	h.mu.Lock()
	blocked := h.SessionBlockedLocked(sid)
	h.mu.Unlock()
	if blocked != nil {
		return "", nil, blocked
	}
	if err := h.ensureStarted(ctx); err != nil {
		return "", nil, err
	}
	h.mu.Lock()
	if err := h.SessionBlockedLocked(sid); err != nil {
		h.mu.Unlock()
		return "", nil, err
	}
	if h.generation != generation {
		h.mu.Unlock()
		return "", nil, fmt.Errorf("agent process changed before prompt")
	}
	if h.active[sid] != 0 || h.opening[sid] != 0 {
		h.mu.Unlock()
		return "", nil, ErrSessionBusy
	}
	caller := h.caller
	caps := h.capabilities
	if err := validatePromptMedia(images, caps); err != nil {
		h.mu.Unlock()
		return "", nil, err
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
			slog.Error(fmt.Sprintf("acphost: cancel notify: %v", err))
		}
		select {
		case <-settled:
		case <-time.After(cancelSettleTimeout):
			// The agent did not end the turn. Give up on a clean stop; the
			// distinct error tells the caller it cannot prove writer quiescence.
			slog.Warn(fmt.Sprintf("acphost: agent did not settle a cancelled turn in %s", cancelSettleTimeout))
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
		return out, activity, h.failedPrompt(ctx, generation, err, fmt.Errorf("agent process changed during prompt"))
	}
	if err != nil {
		if abandoned.Load() {
			return out, activity, promptFailure(errors.Join(err, ctx.Err()), h.ProcessStopped(generation))
		}
		return out, activity, h.failedPrompt(ctx, generation, err, fmt.Errorf("session/prompt: %w", err))
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

// failedPrompt is the error, carrying cause, of a prompt that did not end
// as asked; err is what the prompt's call returned. An answer from the
// agent, an error included, settles the prompt. One that no answer ended
// is settled only by the stop of the agent's process, and the agent's exit
// ends the call before the transport has ended what the agent left in its
// process group: the stop is read once that has settled, waiting for it at
// most exitedGroupWait and no longer than ctx lasts. A group that has not
// emptied by then leaves the stop unconfirmed.
func (h *Host) failedPrompt(ctx context.Context, generation uint64, err, cause error) error {
	if PromptSettled(err) {
		return settledPromptError{cause}
	}
	h.mu.Lock()
	settling := h.settling[generation]
	h.mu.Unlock()
	if settling != nil {
		brief := time.NewTimer(exitedGroupWait)
		defer brief.Stop()
		select {
		case <-settling:
		case <-brief.C:
		case <-ctx.Done():
		}
	}
	return promptFailure(cause, h.ProcessStopped(generation))
}

func validatePromptMedia(images []Image, caps *acp.AgentCapabilities) error {
	for _, media := range images {
		if len(media.Data) == 0 {
			continue
		}
		if caps == nil || caps.PromptCapabilities == nil || (media.URI == "" && !caps.PromptCapabilities.Image) || (media.URI != "" && !caps.PromptCapabilities.EmbeddedContext) {
			return fmt.Errorf("agent does not support the attached media type %s", media.MIME)
		}
	}
	return nil
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
	return h.processStoppedLocked(generation)
}

func (h *Host) processStoppedLocked(generation uint64) bool {
	if generation == 0 || generation > h.generation {
		return false
	}
	proc, exists := h.processes[generation]
	if !exists {
		return true
	}
	if proc.Stopped() {
		delete(h.processes, generation)
		return true
	}
	return false
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
		if !proc.Stopped() {
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
// Wait is never called here — the monitor goroutine is the sole waiter.
// Within the same grace, and the kill that ends it, it also waits for every
// process this host started to settle what it left in its process group;
// one that does not stays unconfirmed. With no agent running there is
// nothing to ask to leave or to kill, so it waits only exitedGroupWait for
// the groups of the agents that exited.
func (h *Host) shutdownLocked() {
	alive := h.alive
	h.alive = false
	conn, stdin, exited, proc := h.conn, h.stdin, h.exited, h.proc
	// Shutdown: the closes tell the agent to leave, and the monitor
	// goroutine reports how it went.
	if alive && conn != nil {
		_ = conn.Close()
	}
	if alive && stdin != nil {
		_ = stdin.Close()
	}
	settling := make([]chan struct{}, 0, len(h.settling))
	for _, settled := range h.settling {
		settling = append(settling, settled)
	}
	running := alive && exited != nil
	if !running && len(settling) == 0 {
		return
	}
	h.mu.Unlock()
	defer h.mu.Lock()
	if !running {
		awaitSettled(settling)
		return
	}
	grace := time.NewTimer(5 * time.Second)
	defer grace.Stop()
	killed := false
	wait := func(done <-chan struct{}) bool {
		for {
			select {
			case <-done:
				return true
			case <-grace.C:
				if killed {
					return false
				}
				killed = true
				proc.Kill()
				grace.Reset(5 * time.Second)
			}
		}
	}
	if !wait(exited) {
		slog.Error("acphost: process did not exit after kill")
		return
	}
	for _, settled := range settling {
		if !wait(settled) {
			slog.Error("acphost: an agent's process group still has members; its stop stays unconfirmed")
			return
		}
	}
}

// exitedGroupWait bounds how long a stop with no agent running waits for
// the process groups of the agents that exited, and how long a prompt that
// no answer ended waits for its agent's. The transport killed what each
// left in its group as the agent exited, and a group the kill ends empties
// within milliseconds, well inside it. One that no kill ends is not waited
// out: its stop is confirmed later, once the group is empty.
const exitedGroupWait = 2 * time.Second

// awaitSettled waits up to exitedGroupWait for the processes whose Wait
// has not returned to settle what they left in their process groups. One
// that has not stays unconfirmed, for ProcessStopped to confirm once its
// group is empty.
func awaitSettled(settling []chan struct{}) {
	brief := time.NewTimer(exitedGroupWait)
	defer brief.Stop()
	for _, settled := range settling {
		select {
		case <-settled:
		case <-brief.C:
			slog.Error("acphost: an exited agent's process group still has members; its stop stays unconfirmed")
			return
		}
	}
}

// applySettings files a session-scoped notification against the session it
// belongs to. These outlive the turn that happens to be running — an agent
// may switch model between turns — so they are recorded on the session
// rather than on the collector.
func (h *Host) applySettings(sid acp.SessionID, u acp.SessionUpdate) {
	h.mu.Lock()
	defer h.mu.Unlock()
	applySessionSettings(h.sessions[sid], u)
}
func applySessionSettings(state *sessionState, u acp.SessionUpdate) {
	applySessionSettingsAt(state, u, 0)
}
func applySessionSettingsAt(state *sessionState, u acp.SessionUpdate, sequence uint64) {
	if state == nil {
		return
	}
	switch u.SessionUpdate {
	case acp.SessionUpdateTypeConfigOptionUpdate:
		values := u.ConfigOptions
		if values == nil {
			values = []acp.SessionConfigOption{}
		}
		state.setOptionsAt(values, sequence)
	case acp.SessionUpdateTypeCurrentModeUpdate:
		state.setModeAt(u.CurrentModeID, sequence)
	case acp.SessionUpdateTypeAvailableCommandsUpdate:
		state.setCommandsAt(u.AvailableCommands, sequence)
	}
}

// sessionSettings returns a reader for the session's current settings. It is
// resolved lazily so a snapshot taken late in a turn sees a model the agent
// switched to mid-turn.
func (h *Host) sessionSettings(sid acp.SessionID) func() view.Settings {
	return func() view.Settings {
		return h.Settings(sid)
	}
}

// Settings reports what the agent last said about how this session is
// configured. It is empty for a session this host does not know.
func (h *Host) Settings(sid acp.SessionID) view.Settings {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.sessions[sid].settings()
	out.Adapter = h.adapter
	return out
}

// SettingsForGeneration returns a single, complete native configuration
// observation. Unknown is distinct from a known session with no selectors:
// process exit must not erase the last confirmed settings on its owner.
// This is not proof that the process is still running or authorized.
func (h *Host) SettingsForGeneration(sid acp.SessionID, generation uint64) (view.Settings, bool) {
	if generation == 0 {
		return view.Settings{}, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	state := h.sessions[sid]
	if h.generation != generation || state == nil {
		return view.Settings{}, false
	}
	out := state.settings()
	out.Adapter = h.adapter
	return out, true
}

// CloseIdle atomically refuses a running prompt before closing its host. It
// is used for explicit retirement of an otherwise idle plugin runtime.
func (h *Host) CloseIdle() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.active) > 0 || len(h.sessionOperations) > 0 || h.newOpening != nil {
		return ErrSessionBusy
	}
	h.isClosed = true
	h.shutdownLocked()
	return nil
}
