// Package agentmcp is the gateway's built-in MCP server: the send primitive
// that lets an agent post interim milestones into the Feishu conversation it
// is working for, before Steve renders the final answer card itself.
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
)

// ServerName is the MCP server name agents see; tool calls arrive as
// feishu_send / feishu_recall under it.
const ServerName = "feishu"

const (
	maxSendsPerTurn = 8
	maxBodyBytes    = 1 << 20
	sendTimeout     = 15 * time.Second
	latestProtocol  = "2025-06-18"
)

// Instructions teaches the agent when to reach for the tools. It is injected
// once per session alongside the rest of the capability instructions, and it
// is part of the capability fingerprint, so keep it stable.
const Instructions = `## Feishu messaging (feishu_send / feishu_recall)
The "feishu" MCP server posts messages into the current Feishu conversation while you work.
- feishu_send is for interim milestones only: a phase conclusion, a produced artifact, a decision worth surfacing early. Most turns need zero interim messages; never narrate step by step.
- Your final answer is delivered automatically by the platform when the turn ends. Do NOT send it with feishu_send, and do not duplicate it there.
- Do not @-mention anyone. Mentions are reserved for the platform's own final answer card.
- feishu_recall deletes a message you sent earlier in this same turn (pass its message_id) if it turned out wrong or obsolete.`

// Sender is the slice of the Feishu channel the tools need. Replies attach
// to the turn's inbound message, which is what keeps a milestone inside the
// topic thread it belongs to.
type Sender interface {
	ReplyCard(ctx context.Context, messageID string, payload []byte) (string, error)
	ReplyText(ctx context.Context, messageID, text string) (string, error)
	DeleteMessage(ctx context.Context, messageID string) error
}

// binding is what a bearer token means: this agent, in this conversation,
// and nowhere else.
type binding struct {
	conversationID string
	agentID        string
}

// anchor is where a conversation's sends currently land. The epoch advances
// with every inbound message, which is what scopes recall to "this turn".
type anchor struct {
	chatID    string
	messageID string
	epoch     uint64
}

// sentState tracks what one agent sent during the current epoch of its
// conversation. A stale epoch means a new turn started; the slate is wiped.
type sentState struct {
	epoch uint64
	ids   map[string]bool
	count int
}

type Server struct {
	listener net.Listener
	srv      *http.Server

	mu      sync.Mutex
	sender  Sender
	tokens  map[string]binding
	byBind  map[binding]string
	anchors map[string]*anchor
	sent    map[binding]*sentState
}

// New binds the loopback listener immediately so the URL is known before any
// capability is assembled. Serving starts with Start.
func New() (*Server, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("agentmcp: listen: %w", err)
	}
	s := &Server{
		listener: listener,
		tokens:   map[string]binding{},
		byBind:   map[binding]string{},
		anchors:  map[string]*anchor{},
		sent:     map[binding]*sentState{},
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

func (s *Server) BindChannel(sender Sender) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sender = sender
}

// Anchor records where the conversation's next sends should attach and
// advances the turn epoch: messages sent before this moment stop being
// recallable, because they belong to a turn that is over.
func (s *Server) Anchor(conversationID, chatID, messageID string) {
	if conversationID == "" || messageID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.anchors[conversationID]
	if a == nil {
		a = &anchor{}
		s.anchors[conversationID] = a
	}
	a.chatID = chatID
	a.messageID = messageID
	a.epoch++
}

// Extras registers the token→(conversation, agent) binding and returns the
// capability to inject into that session's MCP config. A rebind with a new
// token revokes the old one, so a cleared session's credentials do not
// linger.
func (s *Server) Extras(conversationID, agentID, token string) []capability.Extra {
	if s == nil || token == "" || conversationID == "" {
		return nil
	}
	b := binding{conversationID: conversationID, agentID: agentID}
	s.mu.Lock()
	if old, ok := s.byBind[b]; ok && old != token {
		delete(s.tokens, old)
	}
	s.tokens[token] = b
	s.byBind[b] = token
	s.mu.Unlock()
	return []capability.Extra{{
		Name: ServerName,
		Server: capability.MCPServer{
			Type:    "http",
			URL:     s.URL(),
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
	s.mu.Lock()
	bind, authorized := s.tokens[token]
	s.mu.Unlock()
	if token == "" || !authorized {
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
		writeRPCResult(w, req.ID, map[string]any{"tools": toolList()})
	case "tools/call":
		writeRPCResult(w, req.ID, s.callTool(r.Context(), bind, req.Params))
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
		"serverInfo":      map[string]any{"name": "steve-feishu", "version": "0.1.0"},
	}
}

func toolList() []map[string]any {
	return []map[string]any{
		{
			"name": "feishu_send",
			"description": "Post an interim milestone message into the current Feishu conversation. " +
				"Use only for phase conclusions or artifacts worth surfacing before the final answer; " +
				"the final answer itself is delivered by the platform automatically.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"content": map[string]any{
						"type":        "string",
						"description": "Message body. Markdown is rendered as a card.",
					},
					"format": map[string]any{
						"type":        "string",
						"enum":        []string{"markdown", "text"},
						"description": "markdown (default) renders a card; text sends a plain message.",
					},
					"mention": map[string]any{
						"type":        "boolean",
						"description": "Must stay false: mentions are reserved for the platform's final answer card.",
					},
				},
				"required": []string{"content"},
			},
		},
		{
			"name":        "feishu_recall",
			"description": "Recall (delete) a message this agent sent earlier in the current turn via feishu_send.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"message_id": map[string]any{
						"type":        "string",
						"description": "The message_id returned by feishu_send.",
					},
				},
				"required": []string{"message_id"},
			},
		},
	}
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
	case "feishu_send":
		out, err = s.send(ctx, bind, call.Arguments)
	case "feishu_recall":
		out, err = s.recall(ctx, bind, call.Arguments)
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

func (s *Server) send(ctx context.Context, bind binding, rawArgs json.RawMessage) (string, error) {
	var args struct {
		Content string `json:"content"`
		Format  string `json:"format"`
		Mention bool   `json:"mention"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return "", errors.New("bad feishu_send arguments")
	}
	if args.Mention {
		return "", errors.New("mention is not allowed: mentions are reserved for the platform's final answer card")
	}
	switch args.Format {
	case "", "markdown", "text":
	default:
		return "", fmt.Errorf("unknown format %q (use markdown or text)", args.Format)
	}
	content := truncateRunes(stripMentions(args.Content), maxContentRunes)
	if strings.TrimSpace(content) == "" {
		return "", errors.New("content is required")
	}
	s.mu.Lock()
	sender := s.sender
	a := s.anchors[bind.conversationID]
	if sender == nil || a == nil || a.messageID == "" {
		s.mu.Unlock()
		return "", errors.New("no active conversation to deliver to")
	}
	st := s.sent[bind]
	if st == nil {
		st = &sentState{}
		s.sent[bind] = st
	}
	if st.epoch != a.epoch {
		st.epoch = a.epoch
		st.ids = map[string]bool{}
		st.count = 0
	}
	if st.count >= maxSendsPerTurn {
		s.mu.Unlock()
		return "", fmt.Errorf("send limit reached (%d per turn); save the rest for the final answer", maxSendsPerTurn)
	}
	// Reserve the slot before releasing the lock so parallel calls cannot
	// overshoot the cap while a send is in flight.
	st.count++
	anchorID := a.messageID
	epoch := a.epoch
	s.mu.Unlock()

	callCtx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	var id string
	var err error
	if args.Format == "text" {
		id, err = sender.ReplyText(callCtx, anchorID, content)
	} else {
		id, err = sender.ReplyCard(callCtx, anchorID, milestoneCard(content, bind.agentID))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		if st.epoch == epoch && st.count > 0 {
			st.count--
		}
		return "", fmt.Errorf("send failed: %v", err)
	}
	if st.epoch == epoch && id != "" {
		st.ids[id] = true
	}
	return "sent message_id=" + id, nil
}

func (s *Server) recall(ctx context.Context, bind binding, rawArgs json.RawMessage) (string, error) {
	var args struct {
		MessageID string `json:"message_id"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil || strings.TrimSpace(args.MessageID) == "" {
		return "", errors.New("message_id is required")
	}
	s.mu.Lock()
	sender := s.sender
	a := s.anchors[bind.conversationID]
	st := s.sent[bind]
	owned := sender != nil && a != nil && st != nil && st.epoch == a.epoch && st.ids[args.MessageID]
	s.mu.Unlock()
	if !owned {
		return "", errors.New("can only recall a message this agent sent in the current turn")
	}
	callCtx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	if err := sender.DeleteMessage(callCtx, args.MessageID); err != nil {
		return "", fmt.Errorf("recall failed: %v", err)
	}
	s.mu.Lock()
	delete(st.ids, args.MessageID)
	s.mu.Unlock()
	return "recalled " + args.MessageID, nil
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
