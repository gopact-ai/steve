package fleetlab

import (
	"context"
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// The barrier only coordinates test workers. All product state, attempts,
// usage, files and result delivery still travel through Steve's real paths.
type rendezvous struct {
	URL, token string
	server     *http.Server
	done       chan struct{}
	mu         sync.Mutex
	groups     map[string]*arrivals
}
type arrivals struct {
	roles map[string]bool
	ready chan struct{}
}

func newRendezvous(ctx context.Context, gateway string) (*rendezvous, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort(gateway, "0"))
	if err != nil {
		return nil, err
	}
	r := &rendezvous{URL: "http://" + ln.Addr().String(), token: token(16), done: make(chan struct{}), groups: map[string]*arrivals{}}
	r.server = &http.Server{Handler: http.HandlerFunc(r.arrive), ReadHeaderTimeout: 5 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	go func() { defer close(r.done); _ = r.server.Serve(ln) }()
	return r, nil
}

func (r *rendezvous) arrive(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost || subtle.ConstantTimeCompare([]byte(req.Header.Get("Authorization")), []byte("Bearer "+r.token)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	parts := strings.Split(strings.Trim(req.URL.Path, "/"), "/")
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "release-") || (parts[1] != "build" && parts[1] != "docs") {
		http.Error(w, "invalid arrival", 400)
		return
	}
	r.mu.Lock()
	g := r.groups[parts[0]]
	if g == nil {
		g = &arrivals{roles: map[string]bool{}, ready: make(chan struct{})}
		r.groups[parts[0]] = g
	}
	if !g.roles[parts[1]] {
		g.roles[parts[1]] = true
		if len(g.roles) == 2 {
			close(g.ready)
		}
	}
	r.mu.Unlock()
	select {
	case <-g.ready:
		w.WriteHeader(http.StatusNoContent)
	case <-req.Context().Done():
		http.Error(w, "cancelled", http.StatusRequestTimeout)
	}
}
func (r *rendezvous) close() error {
	err := r.server.Close()
	<-r.done
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
