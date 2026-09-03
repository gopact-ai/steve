package readmodel

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"github.com/gopact-ai/steve/internal/nodewire"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ServerConfig is where the read model is served and who may read it.
type ServerConfig struct {
	// Addr defaults to a loopback port. Binding anywhere else requires a
	// token: the snapshot names hosts, goals and agents, and that is not
	// something to hand to the network by accident.
	Addr  string
	Token string
}

// Server exposes the snapshot, the change stream and the dashboard.
type Server struct {
	console  Console
	admin    Admin
	model    *Model
	token    string
	listener net.Listener
}

// NewServer binds immediately so the caller knows the URL before serving.
func NewServer(model *Model, cfg ServerConfig) (*Server, error) {
	addr := strings.TrimSpace(cfg.Addr)
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	if !loopback(addr) && strings.TrimSpace(cfg.Token) == "" {
		return nil, fmt.Errorf("read model on %s needs a token: it reports hosts, goals and agents", addr)
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", addr, err)
	}
	return &Server{model: model, token: cfg.Token, listener: listener}, nil
}

func (s *Server) URL() string { return "http://" + s.listener.Addr().String() }

func (s *Server) Serve() error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /state", s.guard(s.state))
	mux.HandleFunc("GET /events", s.guard(s.events))
	mux.HandleFunc("POST /console/send", s.guard(s.consoleSend))
	mux.HandleFunc("GET /console/replies", s.guard(s.consoleReplies))
	mux.HandleFunc("GET /console/conversations", s.guard(s.consoleConversations))
	mux.HandleFunc("POST /console/nodes", s.guard(s.consoleAddNode))
	mux.HandleFunc("POST /console/agents", s.guard(s.consoleAddAgent))
	mux.HandleFunc("GET /console/nodes/{name}/settings", s.guard(s.nodeSettings))
	mux.HandleFunc("PUT /console/nodes/{name}/settings", s.guard(s.nodeSettings))
	mux.HandleFunc("GET /bootstrap/{name}", s.bootstrap)
	mux.HandleFunc("GET /dist/steve-node", s.nodeBinary)
	mux.HandleFunc("GET /console/context", s.guard(s.consoleContext))
	mux.HandleFunc("GET /console/verbs", s.guard(s.consoleVerbs))
	mux.HandleFunc("GET /console/suggest", s.guard(s.consoleSuggest))
	mux.HandleFunc("GET /history", s.guard(s.history))
	// The bundle is hashed, static code with nothing of the fleet in it,
	// and the browser fetches it without the token the shell was opened
	// with; it is served open. Everything that carries data stays guarded.
	mux.HandleFunc("GET /assets/", s.page)
	mux.HandleFunc("GET /", s.guard(s.page))
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	if err := server.Serve(s.listener); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *Server) Close() error { return s.listener.Close() }

// guard checks the token when one is configured. Loopback-only deployments
// leave it empty and rely on the bind address, which is the same posture the
// messaging server already takes.
func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.token != "" && !s.authorized(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *Server) authorized(r *http.Request) bool {
	presented := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if presented == "" {
		presented = r.URL.Query().Get("token")
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(s.token)) == 1
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	snap := s.model.Snapshot(r.Context())
	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(snap); err != nil {
		log.Printf("readmodel: encode snapshot: %v", err)
	}
}

// events streams changes as server-sent events, so a renderer learns about a
// step transition when it happens instead of on its next poll.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	stream, stop := s.model.Subscribe(r.Context())
	defer stop()

	// Replay what just happened so a renderer attaching mid-flight is not
	// staring at nothing until the next change.
	for _, ev := range s.model.Recent() {
		writeEvent(w, ev)
	}
	flusher.Flush()

	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, open := <-stream:
			if !open {
				return
			}
			writeEvent(w, ev)
			flusher.Flush()
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func writeEvent(w http.ResponseWriter, ev Event) {
	payload, err := json.Marshal(ev)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", payload)
}

func loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "" {
		return false // a bare ":port" listens on every interface
	}
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}

// Console is what the page needs to act, not only to watch: send a line as
// the owner into a conversation and read what came back. The token that
// guards the read model is the owner's credential here; without a
// console wired, the endpoints answer that acting is off.
type Console interface {
	Send(ctx context.Context, conversation, input string) (Reply, error)
	// SendCommand is Send with an idempotency key from the page.
	SendCommand(ctx context.Context, conversation, input, commandID string) (Reply, error)
	Replies(conversation string) []Reply
	// Conversations names every console conversation with a transcript;
	// Summaries describes each one for a sidebar.
	Conversations() []string
	Summaries(ctx context.Context) []Conversation
	// Context is where a conversation stands; Verbs is what it can be told.
	Context(ctx context.Context, conversation string) (Context, error)
	Verbs() []Verb
	// Suggest completes a line the page is typing, by the coordinator's
	// rules: verbs, agents, projects, this conversation's tasks.
	Suggest(ctx context.Context, conversation, line string) []Suggestion
}

// Suggestion is one completion for the line being typed.
type Suggestion struct {
	Label  string `json:"label"`
	Args   string `json:"args,omitempty"`
	Detail string `json:"detail,omitempty"`
	Insert string `json:"insert"`
	Muted  bool   `json:"muted,omitempty"`
}

// Context is a conversation's standing for the page's context bar: its
// project, its current agent, and every agent as a candidate with the
// reason it can or cannot take the next line.
type Context struct {
	Conversation string          `json:"conversation"`
	Project      *ContextProject `json:"project,omitempty"`
	Agent        *AgentChoice    `json:"agent,omitempty"`
	Agents       []AgentChoice   `json:"agents"`
}

type ContextProject struct {
	ID      string `json:"id"`
	Node    string `json:"node"`
	Path    string `json:"path"`
	Level   string `json:"level"`
	Repo    string `json:"repo"`
	Version int64  `json:"version"`
	Bound   bool   `json:"bound"`
}

type AgentChoice struct {
	ID      string `json:"id"`
	Node    string `json:"node"`
	Harness string `json:"harness"`
	Model   string `json:"model,omitempty"`
	Ready   bool   `json:"ready"`
	Why     string `json:"why,omitempty"`
	Usable  bool   `json:"usable"`
	Because string `json:"because,omitempty"`
	Current bool   `json:"current,omitempty"`
}

// Verb is one console verb with its argument shape and a line of help.
type Verb struct {
	Command string `json:"command"`
	Args    string `json:"args,omitempty"`
	Summary string `json:"summary"`
}

// Reply is one exchange on the console.
// AddNodeRequest is the page adding a machine: a name, where the hub
// dials it, and the data level it may handle. HubURL is where the machine
// will fetch its bootstrap from, as the page reached the hub.
type AddNodeRequest struct {
	Name   string `json:"name"`
	Addr   string `json:"addr"`
	Level  string `json:"level,omitempty"`
	HubURL string `json:"-"`
}

// AddNodeResult is what to run on the machine: one command that writes
// its config, fetches the binary if the hub has one, and starts it.
type AddNodeResult struct {
	Name    string `json:"name"`
	Token   string `json:"token"`
	Command string `json:"command"`
	Note    string `json:"note,omitempty"`
}

// AddAgentRequest is the page adding an agent: an id, the AI tool it
// runs, the machine it runs on ("" is the hub), a preferred model.
type AddAgentRequest struct {
	ID      string `json:"id"`
	Harness string `json:"harness"`
	Node    string `json:"node,omitempty"`
	Model   string `json:"model,omitempty"`
}

// Admin changes the fleet at runtime and persists the change: the page
// adds machines and agents without a restart.
type Admin interface {
	AddNode(ctx context.Context, req AddNodeRequest) (AddNodeResult, error)
	AddAgent(ctx context.Context, req AddAgentRequest) error
	// Bootstrap is the script a machine runs, given its own token.
	Bootstrap(name, token string) (string, bool)
	// NodeBinary is the steve-node executable to hand a machine presenting
	// a node token, if the hub has one.
	NodeBinary(token string) (string, bool)
	// NodeSettings reads what a machine offers; SetNodeSettings rewrites
	// it and answers what is in force. The hub machine is one of them.
	NodeSettings(ctx context.Context, name string) (nodewire.Settings, error)
	SetNodeSettings(ctx context.Context, name string, set nodewire.Settings) (nodewire.Settings, error)
}

// Conversation is one console thread as the sidebar lists it: named by
// its first line, placed by its project and agent, and marked while a
// line of it runs.
type Conversation struct {
	ID      string    `json:"id"`
	Title   string    `json:"title"`
	Project string    `json:"project,omitempty"`
	Agent   string    `json:"agent,omitempty"`
	LastAt  time.Time `json:"last_at"`
	Count   int       `json:"count"`
	Running bool      `json:"running"`
}

type Reply struct {
	At           time.Time `json:"at"`
	Conversation string    `json:"conversation"`
	Input        string    `json:"input,omitempty"`
	Title        string    `json:"title,omitempty"`
	Text         string    `json:"text"`
	Error        string    `json:"error,omitempty"`
	Kind         string    `json:"kind"` // reply | milestone | notice
	// Process is how the reply was made, for the page to unfold.
	Process *Process `json:"process,omitempty"`
}

// SetConsole wires the acting half of the page.
func (s *Server) SetConsole(c Console) { s.console = c }

// SetAdmin wires adding machines and agents from the page.
func (s *Server) SetAdmin(a Admin) { s.admin = a }

func (s *Server) consoleAddNode(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.admin == nil {
		http.Error(w, "adding machines is not wired", http.StatusNotImplemented)
		return
	}
	var req AddNodeRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	req.HubURL = scheme + "://" + r.Host
	out, err := s.admin.AddNode(r.Context(), req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Server) nodeSettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.admin == nil {
		http.Error(w, "not wired", http.StatusNotImplemented)
		return
	}
	name := r.PathValue("name")
	var out nodewire.Settings
	var err error
	if r.Method == http.MethodPut {
		var set nodewire.Settings
		if derr := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&set); derr != nil {
			http.Error(w, "bad request: "+derr.Error(), http.StatusBadRequest)
			return
		}
		out, err = s.admin.SetNodeSettings(r.Context(), name, set)
	} else {
		out, err = s.admin.NodeSettings(r.Context(), name)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"settings": out})
}

func (s *Server) consoleAddAgent(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.admin == nil {
		http.Error(w, "adding agents is not wired", http.StatusNotImplemented)
		return
	}
	var req AddAgentRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.admin.AddAgent(r.Context(), req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// bootstrap hands a machine its start script. The machine presents its
// own node token, not the owner's: the script is the machine's business.
func (s *Server) bootstrap(w http.ResponseWriter, r *http.Request) {
	if s.admin == nil {
		http.NotFound(w, r)
		return
	}
	script, ok := s.admin.Bootstrap(r.PathValue("name"), r.URL.Query().Get("token"))
	if !ok {
		http.Error(w, "unknown machine or wrong token", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	_, _ = io.WriteString(w, script)
}

func (s *Server) nodeBinary(w http.ResponseWriter, r *http.Request) {
	if s.admin == nil {
		http.NotFound(w, r)
		return
	}
	path, ok := s.admin.NodeBinary(r.URL.Query().Get("token"))
	if !ok {
		http.Error(w, "no binary here, or wrong token", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(w, r, path)
}

func (s *Server) consoleSend(w http.ResponseWriter, r *http.Request) {
	if s.console == nil {
		http.Error(w, "the console is not enabled on this gateway", http.StatusNotImplemented)
		return
	}
	var req struct {
		Conversation string `json:"conversation"`
		Input        string `json:"input"`
		CommandID    string `json:"command_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Input) == "" {
		http.Error(w, "input is required", http.StatusBadRequest)
		return
	}
	if req.Conversation == "" {
		req.Conversation = "console:main"
	}
	reply, err := s.console.SendCommand(r.Context(), req.Conversation, req.Input, req.CommandID)
	w.Header().Set("Content-Type", "application/json")
	if err != nil && strings.Contains(err.Error(), "already running") {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
		return
	}
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error(), "reply": reply})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"reply": reply})
}

func (s *Server) consoleContext(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.console == nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"enabled": false})
		return
	}
	conversation := r.URL.Query().Get("conversation")
	if conversation == "" {
		conversation = "console:main"
	}
	ctx, err := s.console.Context(r.Context(), conversation)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
		return
	}
	if ctx.Agents == nil {
		ctx.Agents = []AgentChoice{}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"enabled": true, "context": ctx})
}

// history pages what happened, newest first; before is the ledger
// sequence to continue from, as the previous page's next.
func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	entries, next, err := s.model.History(r.Context(), before, limit)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
		return
	}
	if entries == nil {
		entries = []HistoryEntry{}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"entries": entries, "next": next})
}

func (s *Server) consoleSuggest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	items := []Suggestion{}
	if s.console != nil {
		conversation := r.URL.Query().Get("conversation")
		if conversation == "" {
			conversation = "console:main"
		}
		items = append(items, s.console.Suggest(r.Context(), conversation, r.URL.Query().Get("q"))...)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"suggestions": items})
}

func (s *Server) consoleVerbs(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	verbs := []Verb{}
	if s.console != nil {
		verbs = append(verbs, s.console.Verbs()...)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"verbs": verbs})
}

func (s *Server) consoleConversations(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.console == nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"enabled": false, "conversations": []Conversation{}})
		return
	}
	list := s.console.Summaries(r.Context())
	if list == nil {
		list = []Conversation{}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"enabled": true, "conversations": list})
}

func (s *Server) consoleReplies(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.console == nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"enabled": false, "replies": []Reply{}, "conversations": []string{}})
		return
	}
	conversation := r.URL.Query().Get("conversation")
	if conversation == "" {
		conversation = "console:main"
	}
	names := s.console.Conversations()
	if names == nil {
		names = []string{}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"enabled": true, "replies": s.console.Replies(conversation), "conversations": names})
}
