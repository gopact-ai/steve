package readmodel

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
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
