package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gopact-ai/steve/internal/artifact"
	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/i18n"
	"github.com/gopact-ai/steve/internal/nodewire"
	"github.com/gopact-ai/steve/internal/readmodel"
	"github.com/gopact-ai/steve/internal/view"
)

// Model supplies system projections to the transport; it owns no HTTP state.
type Model interface {
	Snapshot(context.Context) readmodel.Snapshot
	Subscribe(context.Context) (<-chan readmodel.Event, func())
	Recent() []readmodel.Event
	History(context.Context, int64, int) ([]readmodel.HistoryEntry, int64, error)
}

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
	coordination consoleapi.CoordinationService
	ssh          SSHService
	sshOrigin    string
	desktop      consoleapi.DesktopService
	mutationMu   sync.RWMutex
	maintenance  bool
	services     consoleapi.ServiceControl
	channels     consoleapi.ChannelsService
	settings     consoleapi.SettingsService
	console      consoleapi.Console
	admin        consoleapi.Admin
	model        Model
	token        string
	listener     net.Listener
	httpServer   *http.Server
	stopRequests context.CancelFunc
}

// NewServer binds immediately so the caller knows the URL before serving.
func NewServer(model Model, cfg ServerConfig) (*Server, error) {
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
	requestContext, stopRequests := context.WithCancel(context.Background())
	server := &http.Server{ReadHeaderTimeout: 10 * time.Second, BaseContext: func(net.Listener) context.Context { return requestContext }}
	return &Server{model: model, token: cfg.Token, listener: listener, httpServer: server, stopRequests: stopRequests}, nil
}

func (s *Server) URL() string { return "http://" + s.listener.Addr().String() }

func (s *Server) Serve() error {
	mux := http.NewServeMux()
	s.sshRoutes(mux)
	s.coordinationRoutes(mux)
	mux.HandleFunc("GET /console/desktop", s.guard(s.consoleDesktop))
	mux.HandleFunc("GET /console/desktop/agents", s.guard(s.consoleDesktopAgents))
	mux.HandleFunc("POST /console/desktop/agents", s.guard(s.consoleDesktopAgents))
	mux.HandleFunc("GET /console/services", s.guard(s.consoleServices))
	mux.HandleFunc("GET /console/services/{name}/restart", s.guard(s.consoleRestart))
	mux.HandleFunc("POST /console/services/{name}/restart", s.guard(s.consoleRestart))
	mux.HandleFunc("GET /console/channels", s.guard(s.consoleChannels))
	mux.HandleFunc("PUT /console/channels", s.guard(s.consoleChannels))
	mux.HandleFunc("GET /console/settings", s.guard(s.consoleSettings))
	mux.HandleFunc("PUT /console/settings", s.guard(s.consoleSettings))
	mux.HandleFunc("PATCH /console/settings", s.guard(s.consoleSettings))
	mux.HandleFunc("GET /state", s.guard(s.state))
	mux.HandleFunc("GET /events", s.guard(s.events))
	mux.HandleFunc("POST /console/send", s.guard(s.consoleSend))
	mux.HandleFunc("POST /console/queue", s.guard(s.consoleEnqueue))
	mux.HandleFunc("GET /console/queue", s.guard(s.consoleQueue))
	mux.HandleFunc("GET /console/questions", s.guard(s.consoleQuestions))
	mux.HandleFunc("GET /console/versions", s.guard(s.consoleVersions))
	mux.HandleFunc("POST /console/questions/{id}/answer", s.guard(s.consoleAnswer))
	s.materialRoutes(mux)
	mux.HandleFunc("DELETE /console/queue/{id}", s.guard(s.consoleDeleteQueued))
	mux.HandleFunc("PATCH /console/queue/{id}", s.guard(s.consoleEditQueued))
	mux.HandleFunc("POST /console/queue/{id}/steer", s.guard(s.consoleSteer))
	mux.HandleFunc("GET /console/replies", s.guard(s.consoleReplies))
	mux.HandleFunc("GET /console/conversations", s.guard(s.consoleConversations))
	mux.HandleFunc("PUT /console/conversations/{id}", s.guard(s.consoleUpdateConversation))
	mux.HandleFunc("POST /console/nodes", s.guard(s.consoleAddNode))
	mux.HandleFunc("DELETE /console/nodes/{name}", s.guard(s.consoleRemoveNode))
	mux.HandleFunc("GET /console/nodes/{name}/agents", s.guard(s.consoleNodeAgents))
	mux.HandleFunc("POST /console/nodes/{name}/agents", s.guard(s.consoleNodeAgents))
	mux.HandleFunc("POST /console/agents", s.guard(s.consoleAddAgent))
	mux.HandleFunc("PUT /console/agents/{id}", s.guard(s.consoleUpdateAgent))
	mux.HandleFunc("DELETE /console/agents/{id}", s.guard(s.consoleRemoveAgent))
	mux.HandleFunc("POST /console/projects", s.guard(s.consoleAddProject))
	mux.HandleFunc("DELETE /console/projects/{id}", s.guard(s.consoleRemoveProject))
	mux.HandleFunc("GET /console/skills", s.guard(s.consoleSkills))
	mux.HandleFunc("GET /console/skills/{name}", s.guard(s.consoleSkill))
	mux.HandleFunc("PUT /console/skills/{name}", s.guard(s.consoleSetSkill))
	mux.HandleFunc("POST /console/skills/paths", s.guard(s.consoleAddSkillPath))
	mux.HandleFunc("DELETE /console/skills/paths", s.guard(s.consoleRemoveSkillPath))
	mux.HandleFunc("GET /console/skills/machines", s.guard(s.consoleMachineSkills))
	mux.HandleFunc("POST /console/skills/machines/refresh", s.guard(s.consoleRefreshMachineSkills))
	mux.HandleFunc("POST /console/skills/import", s.guard(s.consoleImportSkill))
	mux.HandleFunc("POST /console/skills/sources", s.guard(s.consoleAddSkillSource))
	mux.HandleFunc("POST /console/skills/sources/update", s.guard(s.consoleUpdateSkillSources))
	mux.HandleFunc("DELETE /console/skills/sources/{slug}", s.guard(s.consoleRemoveSkillSource))
	mux.HandleFunc("GET /console/mcp", s.guard(s.consoleMCP))
	mux.HandleFunc("POST /console/mcp/probe", s.guard(s.consoleProbeMCP))
	mux.HandleFunc("POST /console/mcp/adopt", s.guard(s.consoleAdoptMCP))
	mux.HandleFunc("DELETE /console/mcp", s.guard(s.consoleRemoveMCP))
	mux.HandleFunc("GET /console/mcp/registry", s.guard(s.consoleMCPRegistry))
	mux.HandleFunc("POST /console/mcp/install", s.guard(s.consoleInstallMCP))
	mux.HandleFunc("GET /console/home", s.guard(s.consoleHome))
	mux.HandleFunc("PUT /console/home/{name}", s.guard(s.consoleSetHomeFile))
	mux.HandleFunc("PUT /console/memory/{project}", s.guard(s.consoleSetProjectMemory))
	mux.HandleFunc("GET /console/selectors", s.guard(s.consoleSelectors))
	mux.HandleFunc("PUT /console/preferences", s.guard(s.consoleSetPreferences))
	mux.HandleFunc("GET /console/tasks/{task}", s.guard(s.consoleTask))
	mux.HandleFunc("PATCH /console/tasks/{task}/meta", s.guard(s.consoleTaskMeta))
	mux.HandleFunc("GET /console/tasks/{task}/attempts", s.guard(s.consoleTaskAttempts))
	mux.HandleFunc("GET /console/attempts/{attempt}/tree", s.guard(s.consoleAttemptTree))
	mux.HandleFunc("GET /console/attempts/{attempt}/file", s.guard(s.consoleAttemptFile))
	mux.HandleFunc("GET /console/attempts/{attempt}/changes", s.guard(s.consoleAttemptChanges))
	mux.HandleFunc("GET /console/attempts/{attempt}/diff", s.guard(s.consoleAttemptDiff))
	mux.HandleFunc("POST /console/projects/{id}/workspaces", s.guard(s.consoleAddWorkspace))
	mux.HandleFunc("DELETE /console/projects/{id}/workspaces/{node}", s.guard(s.consoleRemoveWorkspace))
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
	server := s.httpServer
	server.Handler = mux
	if err := server.Serve(s.listener); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *Server) Close() error {
	if s.stopRequests != nil {
		s.stopRequests()
	}
	var err error
	if s.httpServer != nil {
		err = s.httpServer.Close()
	}
	if s.listener != nil {
		closed := s.listener.Close()
		if closed != nil && !errors.Is(closed, net.ErrClosed) {
			err = errors.Join(err, closed)
		}
	}
	return err
}

// Shutdown cancels streaming request contexts, closes admission, and waits for
// HTTP handlers. Accepted console work has a separate Service.Shutdown barrier.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.stopRequests != nil {
		s.stopRequests()
	}
	if s.httpServer == nil {
		return s.Close()
	}
	err := s.httpServer.Shutdown(ctx)
	if s.listener != nil {
		closed := s.listener.Close()
		if closed != nil && !errors.Is(closed, net.ErrClosed) {
			err = errors.Join(err, closed)
		}
	}
	return err
}

// guard checks the token when one is configured. Loopback-only deployments
// leave it empty and rely on the bind address, which is the same posture the
// messaging server already takes.
func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.token != "" && !s.authorized(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !(strings.HasPrefix(r.URL.Path, "/console/services/") && strings.HasSuffix(r.URL.Path, "/restart")) {
			s.mutationMu.RLock()
			defer s.mutationMu.RUnlock()
			if s.maintenance {
				serviceError(w, &consoleapi.ServiceError{Code: "busy", Message: "The Hub is preparing a service restart"})
				return
			}
		}
		next(w, r.WithContext(i18n.WithLocale(r.Context(), i18n.LocaleFromHeader(r.Header.Get("Accept-Language")))))
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
		slog.Error(fmt.Sprintf("readmodel: encode snapshot: %v", err))
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

func writeEvent(w http.ResponseWriter, ev readmodel.Event) {
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

// SetConsole wires the acting half of the page.
func (s *Server) SetConsole(c consoleapi.Console) { s.console = c }

// SetAdmin wires adding machines and agents from the page.
func (s *Server) SetAdmin(a consoleapi.Admin) { s.admin = a }

func (s *Server) consoleAddNode(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.admin == nil {
		http.Error(w, "adding machines is not wired", http.StatusNotImplemented)
		return
	}
	var req consoleapi.AddNodeRequest
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
	writeJSON(w, out)
}

func (s *Server) consoleRemoveNode(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	if err := s.admin.RemoveNode(r.Context(), r.PathValue("name")); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) consoleAddProject(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.admin == nil {
		http.Error(w, "not wired", http.StatusNotImplemented)
		return
	}
	var req consoleapi.AddProjectRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.admin.AddProject(r.Context(), req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) consoleUpdateAgent(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.admin == nil {
		http.Error(w, "not wired", http.StatusNotImplemented)
		return
	}
	var spec consoleapi.AgentSpec
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&spec); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.admin.UpdateAgent(r.Context(), r.PathValue("id"), spec); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) consoleRemoveAgent(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.admin == nil {
		http.Error(w, "not wired", http.StatusNotImplemented)
		return
	}
	if err := s.admin.RemoveAgent(r.Context(), r.PathValue("id")); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) consoleRemoveProject(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.admin == nil {
		http.Error(w, "not wired", http.StatusNotImplemented)
		return
	}
	if err := s.admin.RemoveProject(r.Context(), r.PathValue("id")); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) consoleAddWorkspace(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.admin == nil {
		http.Error(w, "not wired", http.StatusNotImplemented)
		return
	}
	var req consoleapi.AddWorkspaceRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.admin.AddWorkspace(r.Context(), r.PathValue("id"), req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) consoleRemoveWorkspace(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.admin == nil {
		http.Error(w, "not wired", http.StatusNotImplemented)
		return
	}
	if err := s.admin.RemoveWorkspace(r.Context(), r.PathValue("id"), r.PathValue("node")); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, consoleapi.ErrBusy) {
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// ErrBusy is an admin action refused because something is running where
// it would act.

func (s *Server) adminOr(w http.ResponseWriter) bool {
	w.Header().Set("Content-Type", "application/json")
	if s.admin == nil {
		http.Error(w, "not wired", http.StatusNotImplemented)
		return false
	}
	return true
}

func (s *Server) consoleSkills(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	view, err := s.admin.Skills(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, view)
}

func (s *Server) consoleSkill(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	doc, err := s.admin.SkillContent(r.Context(), r.PathValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, doc)
}

func (s *Server) consoleSetSkill(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.admin.SetSkill(r.Context(), r.PathValue("name"), req.Enabled); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, consoleapi.ErrBusy) {
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) consoleAddSkillPath(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.admin.AddSkillPath(r.Context(), req.Path); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) consoleRemoveSkillPath(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	if err := s.admin.RemoveSkillPath(r.Context(), r.URL.Query().Get("path")); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, consoleapi.ErrBusy) {
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) consoleMachineSkills(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	out := s.admin.MachineSkills(r.Context())
	if out == nil {
		out = []consoleapi.MachineSkills{}
	}
	writeJSON(w, map[string]any{"machines": out})
}

func (s *Server) consoleRefreshMachineSkills(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	out := s.admin.RefreshMachineSkills(r.Context())
	if out == nil {
		out = []consoleapi.MachineSkills{}
	}
	writeJSON(w, map[string]any{"machines": out})
}

func (s *Server) consoleImportSkill(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	var req struct {
		Node string `json:"node"`
		Path string `json:"path"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	name, err := s.admin.ImportSkill(r.Context(), req.Node, req.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "name": name})
}

func (s *Server) consoleAddSkillSource(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	var req struct {
		Spec string `json:"spec"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	src, err := s.admin.AddSkillSource(r.Context(), req.Spec)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, src)
}

func (s *Server) consoleUpdateSkillSources(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	out, err := s.admin.UpdateSkillSources(r.Context())
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, consoleapi.ErrBusy) {
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}
	if out == nil {
		out = []consoleapi.SkillSource{}
	}
	writeJSON(w, map[string]any{"sources": out})
}

func (s *Server) consoleRemoveSkillSource(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	if err := s.admin.RemoveSkillSource(r.Context(), r.PathValue("slug")); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, consoleapi.ErrBusy) {
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) consoleMCP(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	view, err := s.admin.MCP(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, view)
}

func (s *Server) consoleProbeMCP(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	var req struct{ Node, Name string }
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	probe, err := s.admin.ProbeMCP(r.Context(), req.Node, req.Name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": probe.OK, "probe": probe})
}

func (s *Server) consoleAdoptMCP(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	var req struct{ Node, Source, Name string }
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.admin.AdoptMCP(r.Context(), req.Node, req.Source, req.Name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) consoleRemoveMCP(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	if err := s.admin.RemoveMCP(r.Context(), r.URL.Query().Get("node"), r.URL.Query().Get("name")); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) consoleMCPRegistry(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	entries, err := s.admin.SearchMCPRegistry(r.Context(), r.URL.Query().Get("q"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if entries == nil {
		entries = []consoleapi.MCPRegistryEntry{}
	}
	writeJSON(w, map[string]any{"entries": entries})
}

func (s *Server) consoleInstallMCP(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	var req consoleapi.InstallMCPRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.admin.InstallMCP(r.Context(), req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) consoleHome(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	view, err := s.admin.Home(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, view)
}

func (s *Server) consoleSetHomeFile(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	var req struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.admin.SetHomeFile(r.Context(), r.PathValue("name"), req.Text); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) consoleSelectors(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	sel, err := s.admin.Selectors(r.Context(), r.URL.Query().Get("conversation"), r.URL.Query().Get("agent"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if sel.Models == nil {
		sel.Models = []view.Choice{}
	}
	if sel.Options == nil {
		sel.Options = []view.Option{}
	}
	writeJSON(w, sel)
}

func (s *Server) consoleSetPreferences(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	var req struct {
		Conversation string            `json:"conversation"`
		Agent        string            `json:"agent"`
		Patch        map[string]string `json:"patch"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.admin.SetPreferences(r.Context(), req.Conversation, req.Agent, req.Patch); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "note": "下一轮以新会话开始"})
}

// consoleTask joins one task for the page: the read model's task, plan
// and children, and the ledger's attempts through the admin.
func (s *Server) consoleTask(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	id := r.PathValue("task")
	snap := s.model.Snapshot(r.Context())
	var found *readmodel.Task
	for i := range snap.Tasks {
		if snap.Tasks[i].ID == id {
			found = &snap.Tasks[i]
			break
		}
	}
	if found == nil {
		http.Error(w, "no task "+id, http.StatusNotFound)
		return
	}
	detail := TaskDetail{Task: *found, Children: []readmodel.Task{}, Attempts: []consoleapi.AttemptView{}}
	for _, t := range snap.Tasks {
		if t.Parent == id {
			detail.Children = append(detail.Children, t)
		}
	}
	for i := range snap.Plans {
		if snap.Plans[i].TaskID == id {
			p := snap.Plans[i]
			detail.Plan = &p
			break
		}
	}
	if attempts, err := s.admin.TaskAttempts(r.Context(), id); err == nil && attempts != nil {
		detail.Attempts = attempts
	}
	writeJSON(w, detail)
}

func (s *Server) consoleTaskAttempts(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	attempts, err := s.admin.TaskAttempts(r.Context(), r.PathValue("task"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if attempts == nil {
		attempts = []consoleapi.AttemptView{}
	}
	writeJSON(w, attempts)
}

func (s *Server) consoleAttemptTree(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	tree, err := s.admin.AttemptTree(r.Context(), r.PathValue("attempt"), r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if tree.Entries == nil {
		tree.Entries = []artifact.Entry{}
	}
	writeJSON(w, tree)
}

func (s *Server) consoleAttemptFile(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	file, err := s.admin.AttemptFile(r.Context(), r.PathValue("attempt"), r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, file)
}

func (s *Server) consoleAttemptChanges(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	index, err := s.admin.AttemptChanges(r.Context(), r.PathValue("attempt"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if index.Changes == nil {
		index.Changes = []artifact.Change{}
	}
	writeJSON(w, index)
}

func (s *Server) consoleAttemptDiff(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	diff, err := s.admin.AttemptDiff(r.Context(), r.PathValue("attempt"), r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, diff)
}

func (s *Server) consoleSetProjectMemory(w http.ResponseWriter, r *http.Request) {
	if !s.adminOr(w) {
		return
	}
	var req struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.admin.SetProjectMemory(r.Context(), r.PathValue("project"), req.Text); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
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
		if errors.Is(err, nodewire.ErrSettingsRevisionConflict) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"settings": out})
}

func (s *Server) consoleAddAgent(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.admin == nil {
		http.Error(w, "adding agents is not wired", http.StatusNotImplemented)
		return
	}
	var req consoleapi.AddAgentRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.admin.AddAgent(r.Context(), req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
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
	if _, err := io.WriteString(w, script); err != nil {
		slog.Warn(fmt.Sprintf("httpapi: write bootstrap script for %s: %v", r.PathValue("name"), err), "node", r.PathValue("name"))
	}
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
	var req consoleapi.Submission
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Input) == "" && len(req.Refs) == 0 {
		http.Error(w, "input is required", http.StatusBadRequest)
		return
	}
	if req.Conversation == "" {
		req.Conversation = "console:main"
	}
	var reply consoleapi.Reply
	var err error
	if extended, ok := s.console.(consoleapi.Submissions); ok {
		reply, err = extended.SendSubmission(r.Context(), req)
	} else if len(req.Refs) > 0 {
		http.Error(w, "material submission is not supported", http.StatusNotImplemented)
		return
	} else if len(req.Quotes) > 0 {
		reply, err = s.console.SendCommandWith(r.Context(), req.Conversation, req.Input, req.CommandID, req.Quotes)
	} else {
		reply, err = s.console.SendCommand(r.Context(), req.Conversation, req.Input, req.CommandID)
	}
	w.Header().Set("Content-Type", "application/json")
	if errors.Is(err, consoleapi.ErrCommandConflict) {
		w.WriteHeader(http.StatusConflict)
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	if errors.Is(err, consoleapi.ErrConsoleClosing) {
		w.WriteHeader(http.StatusServiceUnavailable)
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		writeJSON(w, map[string]any{"error": err.Error(), "reply": reply})
		return
	}
	writeJSON(w, map[string]any{"reply": reply})
}

func (s *Server) consoleContext(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.console == nil {
		writeJSON(w, map[string]any{"enabled": false})
		return
	}
	conversation := r.URL.Query().Get("conversation")
	if conversation == "" {
		conversation = "console:main"
	}
	ctx, err := s.console.Context(r.Context(), conversation)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	if ctx.Agents == nil {
		ctx.Agents = []consoleapi.AgentChoice{}
	}
	writeJSON(w, map[string]any{"enabled": true, "context": ctx})
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
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	if entries == nil {
		entries = []readmodel.HistoryEntry{}
	}
	writeJSON(w, map[string]any{"entries": entries, "next": next})
}

func (s *Server) consoleSuggest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	items := []consoleapi.Suggestion{}
	if s.console != nil {
		conversation := r.URL.Query().Get("conversation")
		if conversation == "" {
			conversation = "console:main"
		}
		items = append(items, s.console.Suggest(r.Context(), conversation, r.URL.Query().Get("q"))...)
	}
	writeJSON(w, map[string]any{"suggestions": items})
}

func (s *Server) consoleVerbs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	verbs := []consoleapi.Verb{}
	if s.console != nil {
		if localized, ok := s.console.(interface {
			VerbsFor(context.Context) []consoleapi.Verb
		}); ok {
			verbs = append(verbs, localized.VerbsFor(r.Context())...)
		} else {
			verbs = append(verbs, s.console.Verbs()...)
		}
	}
	writeJSON(w, map[string]any{"verbs": verbs})
}

func (s *Server) consoleConversations(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.console == nil {
		writeJSON(w, map[string]any{"enabled": false, "conversations": []consoleapi.Conversation{}})
		return
	}
	list := s.console.Summaries(r.Context())
	if list == nil {
		list = []consoleapi.Conversation{}
	}
	writeJSON(w, map[string]any{"enabled": true, "conversations": list})
}

func (s *Server) consoleUpdateConversation(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.console == nil {
		http.Error(w, "console is not enabled", http.StatusNotImplemented)
		return
	}
	var patch consoleapi.ConversationPatch
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<14)).Decode(&patch); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.console.Update(r.Context(), r.PathValue("id"), patch); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) consoleReplies(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.console == nil {
		writeJSON(w, map[string]any{"enabled": false, "replies": []consoleapi.Reply{}, "conversations": []string{}})
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
	writeJSON(w, map[string]any{"enabled": true, "replies": s.console.Replies(conversation), "conversations": names})
}
