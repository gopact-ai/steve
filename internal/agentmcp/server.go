// Package agentmcp provides session-scoped collaboration tools. Messages
// are addressed to an authorized channel; adapters render and deliver them.
//
// It speaks MCP streamable HTTP (JSON responses, stateless) on a loopback
// listener. Every session gets its own bearer token bound to exactly one
// (conversation, agent) pair, so an agent can never write into someone
// else's chat, and can only recall what it sent itself this turn.
package agentmcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/capability"
	"github.com/gopact-ai/steve/internal/channel"
	"github.com/gopact-ai/steve/internal/task"
)

// ServerName is the MCP server name agents see; tool calls arrive as
// channel_send / channel_recall under it.
const ServerName = "steve"

const (
	maxSendsPerTurn = 8
	// Updates are edits to existing cards, cheaper than sends, but a
	// runaway narrator patching in a loop would hit channel rate limits.
	maxUpdatesPerTurn = 30
	maxBodyBytes      = 1 << 20
	sendTimeout       = 15 * time.Second
	latestProtocol    = "2025-06-18"
)

// Instructions teaches the agent when to reach for the tools. It is injected
// once per session alongside the rest of the capability instructions, and it
// is part of the capability fingerprint, so keep it stable.
const Instructions = `## Steve (steve_context / steve_projects / steve_help)
- Answer greetings and ordinary questions directly from the conversation and supplied context; no Steve tool call is required for every turn.
- Call steve_context when the answer or next action depends on current identity, environment, budget, machines, or available tools that the supplied context does not establish. Check live state before relying on potentially stale operational details; never invent current capabilities.
- Use steve_help(topic) when you need platform usage guidance and steve_projects when you need project locations. Steve's platform capabilities are provided by this MCP server.

## Channel messaging (channel_send / channel_update / channel_recall)
The Steve MCP server posts and maintains interim messages in the current conversation.
- Omit channel to use the current conversation channel. An explicit channel must match its authorized binding; it never grants a new recipient. steve_context reports the bound channel.
- Pass the opaque message_id from channel_send to update or recall; the receipt fixes its channel and destination.
- channel_send posts a milestone: a phase conclusion, a produced artifact, a decision worth surfacing early. Most turns need zero interim messages; never narrate step by step.
- Prefer ONE evolving progress message per task: channel_update(message_id, content) rewrites a message you sent earlier this turn. Update it as phases complete instead of sending a new message each time.
- On multi-stage work pass progress like "2/3" (stage/total) to channel_send and channel_update to identify the current stage.
- Your final answer is delivered automatically by the platform when the turn ends. Do NOT send it with these tools, and do not duplicate it there.
- Do not @-mention anyone. Mentions are reserved for the platform's own final answer.
- channel_recall deletes a message you sent earlier in this same turn (pass its message_id) if it turned out wrong or obsolete.

## Delegation (steve_delegate / steve_await, when offered)
- steve_delegate hands ONE bounded goal to another agent — possibly on another machine. Use it when the work needs a machine, credential or environment you do not have.
- It returns quickly with a task_id and a state. While state is running, call steve_await(task_id) — it waits up to 50 seconds per call — until state is done or failed. Then report the task_id, agent, node and outcome it gives you; do not invent them.
- Pass refs (a commit, a branch, a blob digest), never pasted content. The child starts a fresh session with only what you give it.
- The child spends your task's budget. Delegate what you cannot do yourself, not what you would rather not.`

// binding is what a bearer token means: this agent, in this conversation,
// and nowhere else. A delegated child carries its task and who delegated it,
// so its token is its own — it cannot pose as the parent — and its cards say
// on whose behalf they were sent.
type binding struct {
	conversationID string
	agentID        string
	taskID         string
	delegatedBy    string
}

// DelegateRequest is what an agent asks for with steve_delegate.
type DelegateRequest struct {
	Agent    string   `json:"agent,omitempty"`
	Requires []string `json:"requires,omitempty"`
	Goal     string   `json:"goal"`
	Refs     []string `json:"refs,omitempty"`
	Expect   string   `json:"expect,omitempty"`
}

// DelegateResult is what comes back: where the work went, whether it is
// still running, and when it is done the answer, the refs it produced, and
// how it ended. Never the child's transcript — that stays with the child.
type DelegateResult struct {
	TaskID string `json:"task_id"`
	Agent  string `json:"agent"`
	Node   string `json:"node,omitempty"`
	// State is running, done or failed. A delegation can outlive any one
	// tool call, so the caller reads State and awaits when it is running.
	State   task.State   `json:"state"`
	Elapsed string       `json:"elapsed,omitempty"`
	Outcome task.Outcome `json:"outcome,omitempty"`
	Answer  string       `json:"answer,omitempty"`
	Refs    []string     `json:"refs,omitempty"`
	// Note tells the caller what happens next while the child runs.
	Note string `json:"note,omitempty"`
}

// AwaitRequest asks for a child's result, waiting up to WaitSeconds.
type AwaitRequest struct {
	TaskID      string `json:"task_id"`
	WaitSeconds int    `json:"wait_seconds,omitempty"`
}

// Delegator opens a child task for the calling agent's running task and
// drives it. Start returns as soon as the child is placed and running (or
// sooner done); Await waits for it. The split exists because a child can
// take minutes and a tool call cannot: a client that times out a request
// must not take the child down with it. The gateway wires one; without it
// the tools are not offered at all rather than offered and refused.
type Delegator interface {
	Start(ctx context.Context, conversationID, agentID string, req DelegateRequest) (DelegateResult, error)
	Await(ctx context.Context, conversationID, agentID string, req AwaitRequest) (DelegateResult, error)
	// Fleet says who else there is and what each can do, so an agent
	// deciding to delegate can name a requirement that exists.
	Fleet(ctx context.Context, conversationID, agentID string, requires []string) (string, error)
}

// anchor is where a conversation's sends currently land. The epoch advances
// with every inbound message, which is what scopes recall to "this turn".
type anchor struct {
	address channel.Address
	epoch   uint64
}

// sentState tracks what one agent sent during the current epoch of its
// conversation. A stale epoch means a new turn started; the slate is wiped.
// sentMsg is what the server remembers about one message an agent sent
// this turn: enough to authorize recall/update and to keep the card's
// stage badge stable across updates.
type sentMsg struct {
	address         channel.Address
	messenger       channel.Messenger
	attribution     string
	busy            bool
	version         uint64
	format          string
	seq             int
	progress        string
	intentID        string
	pendingTool     string
	pendingProgress string
}

type sentState struct {
	epoch uint64
	// ids maps each message this agent sent this turn to its record;
	// updates only apply to markdown cards, recall applies to all.
	ids     map[string]sentMsg
	count   int
	updates int
}

type Server struct {
	intents  Intents
	listener net.Listener
	srv      *http.Server

	mu                  sync.Mutex
	channels            map[string]channel.Messenger
	defaultChannel      string
	delegator           Delegator
	informer            Informer
	fleeter             Fleeter
	memorizer           Memorizer
	journal             func(conversationID, agentID, taskID string, receipt channel.Address)
	tokens              map[string]binding
	byBind              map[binding]string
	anchors             map[string]*anchor
	sent                map[binding]*sentState
	styles              map[string]string
	effects             map[string]bool
	store               Store
	storeErr            error
	onFailure           func(error)
	interims            map[string]bool
	loadedConversations map[string]bool
	loadedMessages      map[binding]bool
}

// New binds the loopback listener immediately so the URL is known before any
// capability is assembled. Serving starts with Start.
//
// The URL is part of every session's capability fingerprint and lives inside
// resumed agent sessions, so the port must survive gateway restarts: pass
// the previously used port to bind it again. 0 (or a port meanwhile taken)
// falls back to an ephemeral one — existing sessions then drift and ask for
// /new, which is the honest outcome.
func New(preferredPort int) (*Server, error) {
	var listener net.Listener
	var err error
	if preferredPort > 0 {
		listener, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", preferredPort))
	}
	if listener == nil {
		listener, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		return nil, fmt.Errorf("agentmcp: listen: %w", err)
	}
	s := &Server{
		listener:            listener,
		channels:            map[string]channel.Messenger{},
		tokens:              map[string]binding{},
		byBind:              map[binding]string{},
		anchors:             map[string]*anchor{},
		sent:                map[binding]*sentState{},
		styles:              map[string]string{},
		interims:            map[string]bool{},
		loadedConversations: map[string]bool{},
		loadedMessages:      map[binding]bool{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", s.handleMCP)
	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	return s, nil
}

// Start serves until ctx is done. It always returns nil after a clean
// shutdown so callers can run it in a bare goroutine.
func (s *Server) Start(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(shutdown)
	}()
	if err := s.srv.Serve(s.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("agentmcp: serve: %w", err)
	}
	return nil
}

// URL is the endpoint injected into agent MCP configs.
func (s *Server) URL() string {
	return "http://" + s.listener.Addr().String() + "/mcp"
}

// Port reports the bound port, for the caller to persist and hand back to
// New on the next start.
// Addr is the loopback listener itself, for a forwarder that has to reach
// this server without going through a URL.
func (s *Server) Addr() string { return s.listener.Addr().String() }

func (s *Server) Port() int {
	addr, ok := s.listener.Addr().(*net.TCPAddr)
	if !ok {
		return 0
	}
	return addr.Port
}

// BindChannel registers a transport adapter. Configuration chooses the default;
// the adapter registry never infers destinations from message ID prefixes.
func (s *Server) BindChannel(name string, messenger channel.Messenger) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.channels[name] = messenger
}

func (s *Server) SetDefaultChannel(name string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.defaultChannel = name
}

// Anchor binds the current turn to an authorized channel destination.
// A missing Channel uses the configured default; no destination is invented.
func (s *Server) Anchor(conversationID string, address channel.Address) {
	if conversationID == "" || address.Conversation == "" || address.Message == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadConversationLocked(context.Background(), conversationID, nil); err != nil {
		return
	}
	if address.Channel == "" {
		address.Channel = s.defaultChannel
	}
	a := s.anchors[conversationID]
	if a != nil && a.address == address {
		return
	}
	if a == nil {
		a = &anchor{}
		s.anchors[conversationID] = a
	}
	a.address = address
	a.epoch++
	s.interims[conversationID] = false
	_ = s.saveConversationLocked(context.Background(), conversationID)
}

// SetJournal registers a callback for every message an agent sends, so the
// platform can persist what would otherwise be in-memory only — and clean
// it up if the turn dies with the process.
// SetDelegator enables steve_delegate.
func (s *Server) SetDelegator(d Delegator) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delegator = d
}

// Delegated mints the capability for a child task: its own token, bound to
// the child, so nothing it sends can be mistaken for the parent's, and its
// milestone cards carry the attribution.
func (s *Server) Delegated(conversationID, agentID, taskID, delegatedBy, token, endpoint string) []capability.Extra {
	if s == nil || token == "" || conversationID == "" {
		return nil
	}
	if endpoint == "" {
		endpoint = s.URL()
	}
	b := binding{conversationID: conversationID, agentID: agentID, taskID: taskID, delegatedBy: delegatedBy}
	s.mu.Lock()
	err := s.prepareLocked(b, token)
	s.mu.Unlock()
	if err != nil {
		return nil
	}
	return []capability.Extra{{
		Name: ServerName,
		Server: capability.MCPServer{
			Type:    "http",
			URL:     endpoint,
			Headers: map[string]string{"Authorization": "Bearer " + token},
		},
		Instructions: Instructions,
	}}
}

// Revoke forgets a token once the work it authorised is over. A child task
// ends; its token must not outlive it.
func (s *Server) Revoke(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.revokeLocked(token)
}

func (s *Server) SetJournal(journal func(conversationID, agentID, taskID string, receipt channel.Address)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.journal = journal
}

// SetStyle records the identity line the platform's own cards wear
// ("codex · GPT 5.6 Sol · Agent"); interim cards use it as their tail so
// every card of a turn reads as one family.
func (s *Server) SetStyle(conversationID, style string) {
	if s == nil || conversationID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadConversationLocked(context.Background(), conversationID, nil); err != nil {
		return
	}
	if strings.TrimSpace(style) == "" {
		delete(s.styles, conversationID)
	} else {
		s.styles[conversationID] = style
	}
	_ = s.saveConversationLocked(context.Background(), conversationID)
}

// Interim reports whether any agent posted messages into the conversation
// during the current turn epoch — the signal that the final answer must be
// posted below them rather than patched into the opening card.
func (s *Server) Interim(conversationID string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadConversationLocked(context.Background(), conversationID, nil); err != nil {
		return false
	}
	if s.interims[conversationID] {
		return true
	}
	a := s.anchors[conversationID]
	if a == nil {
		return false
	}
	for b, st := range s.sent {
		if b.conversationID == conversationID && st.epoch == a.epoch && st.count > 0 {
			return true
		}
	}
	return false
}

// Extras registers the token→(conversation, agent) binding and returns the
// capability to inject into that session's MCP config. A rebind with a new
// token revokes the old one, so a cleared session's credentials do not
// linger.
// Extras injects the messaging capability for one session. endpoint is the
// URL *the agent* should call: empty for an agent on the hub, and the node's
// own loopback port for a remote one, which the node forwards back here over
// the connection it already has. Either way the agent only ever talks to
// 127.0.0.1 on its own machine, so "loopback only, one bearer token per
// session" survives the move to another host.
func (s *Server) Extras(conversationID, agentID, token, endpoint string) []capability.Extra {
	if s == nil || token == "" || conversationID == "" {
		return nil
	}
	if endpoint == "" {
		endpoint = s.URL()
	}
	b := binding{conversationID: conversationID, agentID: agentID}
	s.mu.Lock()
	err := s.prepareLocked(b, token)
	s.mu.Unlock()
	if err != nil {
		return nil
	}
	return []capability.Extra{{
		Name: ServerName,
		Server: capability.MCPServer{
			Type:    "http",
			URL:     endpoint,
			Headers: map[string]string{"Authorization": "Bearer " + token},
		},
		Instructions: Instructions,
	}}
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// Streamable HTTP allows a server with no listening stream to
		// refuse GET (and session DELETE) with 405.
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	_, _, authErr := s.authenticate(r.Context(), token, false)
	if token == "" || authErr != nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		// Includes batches: nothing Steve injects sends them.
		writeRPCError(w, json.RawMessage("null"), -32700, "parse error")
		return
	}
	if len(req.ID) == 0 || string(req.ID) == "null" {
		// A notification (notifications/initialized and friends) is
		// acknowledged and dropped.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch req.Method {
	case "initialize":
		writeRPCResult(w, req.ID, initializeResult(req.Params))
	case "ping":
		writeRPCResult(w, req.ID, struct{}{})
	case "tools/list":
		writeRPCResult(w, req.ID, map[string]any{"tools": s.toolList()})
	case "tools/call":
		bind, callCtx, err := s.authenticate(r.Context(), token, true)
		if err != nil {
			http.Error(w, `{"error":"unauthorized execution"}`, http.StatusUnauthorized)
			return
		}
		writeRPCResult(w, req.ID, s.callTool(callCtx, bind, req.Params))
	default:
		writeRPCError(w, req.ID, -32601, fmt.Sprintf("method %q not found", req.Method))
	}
}

func initializeResult(params json.RawMessage) map[string]any {
	var requested struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &requested)
	version := latestProtocol
	switch requested.ProtocolVersion {
	case "2024-11-05", "2025-03-26", "2025-06-18":
		version = requested.ProtocolVersion
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "steve", "version": "0.1.0"},
	}
}

// PlatformTool is one of the platform's own tools, for a page.
type PlatformTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// PlatformTools lists the tools the platform's session server offers:
// the channel messaging set always, delegation when a delegator is wired.
func PlatformTools(delegating bool) []PlatformTool {
	var out []PlatformTool
	for _, t := range toolList(delegating, true, true, true) {
		name, _ := t["name"].(string)
		desc, _ := t["description"].(string)
		out = append(out, PlatformTool{Name: name, Description: desc})
	}
	return out
}

func (s *Server) toolList() []map[string]any {
	s.mu.Lock()
	informing, fleeting, remembering := s.informer != nil, s.fleeter != nil, s.memorizer != nil
	delegating := s.delegator != nil
	s.mu.Unlock()
	return toolList(delegating, informing, fleeting, remembering)
}

// titles are the short labels the console shows for the platform's own
// tools, kept beside the tools themselves: adding a tool means adding
// its label here, and the page never names a tool on its own.
var titles = map[string]string{
	"steve_context": "看当前上下文", "steve_help": "查平台用法", "steve_projects": "查项目", "steve_fleet": "查名册",
	"steve_delegate": "委派子任务", "steve_await": "等子任务",
	"steve_remember": "记一条记忆", "steve_recall": "查记忆", "steve_forget": "忘一条记忆",
	"channel_send": "发进度消息", "channel_update": "改进度消息", "channel_recall": "撤回消息",
	"steve_nodes": "查机器", "steve_node_add": "登记机器", "steve_node_refresh": "刷新机器", "steve_node_remove": "移除机器",
}

// ToolTitles is every tool the platform can offer, with its label: the
// read model uses it to recognise the platform's own calls under any
// harness's naming. A tool without a label is still recognised, by name.
func ToolTitles() map[string]string {
	out := map[string]string{}
	for _, t := range toolList(true, true, true, true) {
		name, _ := t["name"].(string)
		if name == "" {
			continue
		}
		if title, ok := titles[name]; ok {
			out[name] = title
		} else {
			out[name] = strings.ReplaceAll(strings.TrimPrefix(name, "steve_"), "_", " ")
		}
	}
	return out
}

func toolList(delegating, informing, fleeting, remembering bool) []map[string]any {
	tools := []map[string]any{
		{
			"name": "channel_send",
			"description": "Post an interim milestone message into the current channel conversation. " +
				"Use only for phase conclusions or artifacts worth surfacing before the final answer; " +
				"the final answer itself is delivered by the platform automatically.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"channel": channelArgument(),
					"content": map[string]any{
						"type":        "string",
						"description": "Message body. The channel renders Markdown or plain text.",
					},
					"format": map[string]any{
						"type":        "string",
						"enum":        []string{"markdown", "text"},
						"description": "markdown (default) or plain text; the channel owns rendering.",
					},
					"mention": map[string]any{
						"type":        "boolean",
						"description": "Must stay false: mentions are reserved for the platform's final answer.",
					},
					"progress": map[string]any{
						"type":        "string",
						"description": "Optional stage badge like \"2/3\" for multi-stage work.",
					},
				},
				"required": []string{"content"},
			},
		},
		{
			"name": "channel_update",
			"description": "Rewrite a message this agent sent earlier in the current turn via channel_send. " +
				"Prefer one evolving progress message over many separate sends.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"channel": channelArgument(),
					"message_id": map[string]any{
						"type":        "string",
						"description": "The message_id returned by channel_send.",
					},
					"content": map[string]any{
						"type":        "string",
						"description": "The full replacement message body, using the original format.",
					},
					"progress": map[string]any{
						"type":        "string",
						"description": "Optional new stage badge like \"3/3\".",
					},
				},
				"required": []string{"message_id", "content"},
			},
		},
		{
			"name": "steve_fleet",
			"description": "List the other agents in the fleet with the machine each runs on and what that machine can do " +
				"(harnesses, models, MCP servers, tools, hardware, networks, credentials), in the selector form " +
				"steve_delegate's requires accepts (tool:docker, mcp:github, hardware:gpu, model:claude*, network:internal). " +
				"Call this before delegating by capability, so the requirement names something that exists. " +
				"Pass requires to see who meets a requirement and what the others lack.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"requires": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Optional selectors, same form as steve_delegate's requires; every agent is judged against them.",
				},
			}},
		},
		{
			"name":        "channel_recall",
			"description": "Recall (delete) a message this agent sent earlier in the current turn via channel_send.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"channel": channelArgument(),
					"message_id": map[string]any{
						"type":        "string",
						"description": "The message_id returned by channel_send.",
					},
				},
				"required": []string{"message_id"},
			},
		},
	}
	if delegating {
		tools = append(tools, map[string]any{
			"name": "steve_delegate",
			"description": "Hand one bounded piece of work to another agent, possibly on another machine. " +
				"Name the agent, or say what capability the work needs (gpu, internal-net, prod-cred) and Steve picks who can. " +
				"Returns as soon as the child is placed: read `state`. If it is `running`, you do not have to wait: when the child ends, Steve sends its result into this conversation as a new message, and you continue from there — so end your turn when nothing else is left. " +
				"Call steve_await only when you must have the result within this turn. " +
				"The child gets its own budget carved from yours, its own session, and only what you pass here — never your transcript. " +
				"Use for work that needs a machine or credential you do not have; not for splitting work you could do yourself.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"goal": map[string]any{
						"type":        "string",
						"description": "One bounded goal, stated so someone with none of your context could act on it.",
					},
					"agent": map[string]any{
						"type":        "string",
						"description": "A specific agent id. Leave empty to place by capability.",
					},
					"requires": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Capabilities the machine must have, e.g. [\"gpu\"]. Used when agent is empty.",
					},
					"refs": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Pointers the child needs: \"git <commit-or-branch>\", \"blob <digest>\". Pass refs, not content.",
					},
					"expect": map[string]any{
						"type":        "string",
						"description": "What a good result looks like, in one line. Becomes the child's acceptance line.",
					},
				},
				"required": []string{"goal"},
			},
		}, map[string]any{
			"name": "steve_await",
			"description": "Wait for a delegated child task and return its result. Usually unnecessary: a child's result is delivered into this conversation as a message when it ends. " +
				"Use it only when you need the result within this turn. Waits up to wait_seconds (max 50) and returns `state: running` if it is not finished yet. Only the caller's own children can be awaited.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"task_id":      map[string]any{"type": "string", "description": "The task_id steve_delegate returned."},
					"wait_seconds": map[string]any{"type": "integer", "description": "How long to wait this call, 1–50. Default 30."},
				},
				"required": []string{"task_id"},
			},
		})
	}
	if informing {
		tools = append(tools, informTools()...)
	}
	if fleeting {
		tools = append(tools, fleetTools()...)
	}
	if remembering {
		tools = append(tools, memoryTools()...)
	}
	return tools
}

func (s *Server) callTool(ctx context.Context, bind binding, params json.RawMessage) map[string]any {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil {
		return toolError("bad tools/call params")
	}
	var out string
	var err error
	switch call.Name {
	case "channel_send", "channel_update", "channel_recall":
		out, err = s.channelCall(ctx, bind, call.Name, call.Arguments)
	case "steve_fleet":
		out, err = s.fleet(ctx, bind, call.Arguments)
	case "steve_delegate":
		out, err = s.delegate(ctx, bind, call.Arguments)
	case "steve_await":
		out, err = s.await(ctx, bind, call.Arguments)
	case "steve_context":
		out, err = s.steveContext(ctx, bind)
	case "steve_projects":
		out, err = s.steveProjects(ctx, bind)
	case "steve_help":
		out, err = s.steveHelp(call.Arguments)
	case "steve_nodes":
		out, err = s.steveNodes(ctx, bind)
	case "steve_node_add":
		out, err = s.steveNodeAdd(ctx, bind, call.Arguments)
	case "steve_node_remove":
		out, err = s.steveNodeRemove(ctx, bind, call.Arguments)
	case "steve_node_refresh":
		out, err = s.steveNodeRefresh(ctx, bind, call.Arguments)
	case "steve_remember":
		out, err = s.steveRemember(ctx, bind, call.Arguments)
	case "steve_recall":
		out, err = s.steveRecall(ctx, bind, call.Arguments)
	case "steve_forget":
		out, err = s.steveForget(ctx, bind, call.Arguments)
	default:
		err = fmt.Errorf("unknown tool %q", call.Name)
	}
	if err != nil {
		log.Printf("agentmcp: %s conversation=%s agent=%s refused: %v", call.Name, bind.conversationID, bind.agentID, err)
		return toolError(err.Error())
	}
	log.Printf("agentmcp: %s conversation=%s agent=%s ok", call.Name, bind.conversationID, bind.agentID)
	return map[string]any{"content": []map[string]any{{"type": "text", "text": out}}}
}

func toolError(text string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": true,
	}
}

// fleet answers "who is there and what can they do" from the roster, in
// the same selector vocabulary steve_delegate's requires uses.
func (s *Server) fleet(ctx context.Context, bind binding, rawArgs json.RawMessage) (string, error) {
	s.mu.Lock()
	delegator := s.delegator
	s.mu.Unlock()
	if delegator == nil {
		return "", errors.New("delegation is not enabled on this gateway")
	}
	var args struct {
		Requires []string `json:"requires"`
	}
	if len(rawArgs) > 0 {
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("steve_fleet: %w", err)
		}
	}
	return delegator.Fleet(ctx, bind.conversationID, bind.agentID, args.Requires)
}

// delegate hands work to another agent and blocks until it is done. The
// result is a typed summary — answer, refs, outcome — not the child's
// transcript: what the child saw stays inspectable on the child's task, and
// what comes back is bounded.
func (s *Server) delegate(ctx context.Context, bind binding, raw json.RawMessage) (string, error) {
	s.mu.Lock()
	d := s.delegator
	s.mu.Unlock()
	if d == nil {
		return "", fmt.Errorf("delegation is not enabled on this gateway")
	}
	var req DelegateRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return "", fmt.Errorf("bad steve_delegate arguments: %w", err)
	}
	if strings.TrimSpace(req.Goal) == "" {
		return "", fmt.Errorf("steve_delegate needs a goal")
	}
	result, err := d.Start(ctx, bind.conversationID, bind.agentID, req)
	if err != nil {
		return "", err
	}
	out, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// await returns a child's result, waiting a bounded time for it. Bounded so
// the tool call returns before any client's timeout; the caller calls again
// while State is still running.
func (s *Server) await(ctx context.Context, bind binding, raw json.RawMessage) (string, error) {
	s.mu.Lock()
	d := s.delegator
	s.mu.Unlock()
	if d == nil {
		return "", fmt.Errorf("delegation is not enabled on this gateway")
	}
	var req AwaitRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return "", fmt.Errorf("bad steve_await arguments: %w", err)
	}
	if strings.TrimSpace(req.TaskID) == "" {
		return "", fmt.Errorf("steve_await needs the task_id steve_delegate returned")
	}
	result, err := d.Await(ctx, bind.conversationID, bind.agentID, req)
	if err != nil {
		return "", err
	}
	out, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// milestoneTail builds the interim card's footer: the same identity line
// the final card wears, plus which stage this is. A delegated child's tail
// names who delegated it, so a card from another machine is not mistaken
// for the parent's own progress.
func milestoneTail(style, agentID string, seq int, progress string) string {
	base := style
	if base == "" {
		base = agentID
	}
	label := fmt.Sprintf("里程碑 %d", seq)
	if progress != "" {
		label = "里程碑 " + progress
	}
	if base == "" {
		return label
	}
	return base + " · " + label
}

func writeRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func writeRPCError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	writeJSON(w, map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": message},
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("agentmcp: write response: %v", err)
	}
}

// Intents is the side-effect ledger: every agent-made call is claimed by
// the attempt, journaled as dispatched before it leaves, and confirmed or
// marked unknown after. A call an earlier attempt made and never heard
// back about blocks the same call until a person resolves it.
type Intents interface {
	Claim(ctx context.Context, taskID, tool string, args []byte) (string, error)
	Dispatched(ctx context.Context, id string) error
	Confirmed(ctx context.Context, id string, receipt any) error
	Failed(ctx context.Context, id string, cause error) error
	Lost(ctx context.Context, id string, cause error) error
}

// SetIntents wires the side-effect ledger.
func (s *Server) SetIntents(i Intents) { s.mu.Lock(); defer s.mu.Unlock(); s.intents = i }

// effect runs one side-effecting tool under the intent protocol.
type intentContextKey struct{}

func (s *Server) effect(ctx context.Context, bind binding, tool string, args json.RawMessage, call func(context.Context) (string, channel.Address, error)) (string, error) {
	key := bind.taskID + "\x00" + bind.conversationID + "\x00" + tool + "\x00" + string(args)
	s.mu.Lock()
	intents := s.intents
	if s.effects == nil {
		s.effects = map[string]bool{}
	}
	if s.effects[key] {
		s.mu.Unlock()
		return "", errors.New("the same message operation is already in progress")
	}
	s.effects[key] = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.effects, key); s.mu.Unlock() }()
	if intents == nil {
		out, _, err := call(ctx)
		return out, err
	}
	var id string
	var err error
	if scope, fixed := ScopeFromContext(ctx); fixed {
		claims, ok := intents.(interface {
			ClaimExecution(context.Context, string, string, string, []byte) (string, error)
		})
		if !ok || scope.TaskID != bind.taskID {
			return "", errors.New("fixed execution intent claims are unavailable")
		}
		id, err = claims.ClaimExecution(ctx, scope.TaskID, scope.AttemptID, tool, args)
	} else {
		id, err = intents.Claim(ctx, bind.taskID, tool, args)
	}
	if err != nil {
		return "", err
	}
	if err := intents.Dispatched(ctx, id); err != nil {
		return "", fmt.Errorf("record intent: %w", err)
	}
	out, receipt, err := call(context.WithValue(ctx, intentContextKey{}, id))
	// A disconnected caller must not erase an external effect's receipt.
	journalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sendTimeout)
	defer cancel()
	var journalErr error
	switch {
	case err == nil:
		journalErr = intents.Confirmed(journalCtx, id, map[string]any{"result": out, "message": receipt})
	case ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || errors.Is(err, errOutcomeUnknown):
		journalErr = intents.Lost(journalCtx, id, err)
	default:
		journalErr = intents.Failed(journalCtx, id, err)
	}
	if journalErr != nil {
		return "", fmt.Errorf("%w: could not persist result for intent %s: %v", errOutcomeUnknown, id, journalErr)
	}
	return out, err
}
