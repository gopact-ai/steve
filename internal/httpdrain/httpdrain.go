// Package httpdrain runs an http.Server that counts as stopped only once
// every handler it started has returned.
//
// http.Server.Serve returns as soon as its listeners close, Shutdown gives up
// at its deadline with handlers still running, and Close does not wait for
// them at all. An owner that closes storage after any of those can have a
// handler write to it after it closed. Here Serve, Shutdown and Close all
// return only after the last handler has.
package httpdrain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// stuckReportAfter is how long a stop waits on handlers it has cut short
// before it says which requests it is still waiting for.
const stuckReportAfter = time.Second

// request is a handler running: what it serves and since when.
type request struct {
	method, path string
	since        time.Time
}

// Server wraps one http.Server. Its Handler is read when Serve starts, so it
// may be set until then.
type Server struct {
	srv *http.Server

	// force is cancelled when a stop stops waiting for handlers to finish
	// on their own; every request context is cancelled with it.
	force context.Context
	abort context.CancelFunc

	mu       sync.Mutex
	stopping bool
	// addr is the address Serve listens on, for the stop to name.
	addr string
	next uint64
	// running holds the handlers running, keyed in the order they started.
	running map[uint64]request
	// idle is closed once a stop has begun and no handler is running.
	idle chan struct{}

	once    sync.Once
	stopped chan struct{}
	// cut is the stop's context error when handlers were cut short; err
	// is what closing the listeners and connections reported.
	cut, err error
}

func New(srv *http.Server) *Server {
	force, abort := context.WithCancel(context.Background())
	return &Server{srv: srv, force: force, abort: abort, running: map[uint64]request{}, idle: make(chan struct{}), stopped: make(chan struct{})}
}

// Serve serves listener, which it closes, until the server is stopped, and
// returns once the stop has finished. A stop by Shutdown or Close returns
// nil. Any other end of serving stops the server as Close does before its
// error is returned. Serve is called at most once.
func (s *Server) Serve(listener net.Listener) error {
	next := s.srv.Handler
	if next == nil {
		next = http.DefaultServeMux
	}
	s.srv.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.handle(next, w, r) })
	s.mu.Lock()
	s.addr = listener.Addr().String()
	s.mu.Unlock()
	err := s.srv.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		<-s.stopped
		return nil
	}
	_ = s.Close()
	return err
}

// Shutdown stops accepting, lets handlers finish until ctx ends, then
// cancels their contexts, closes their connections and waits for them to
// return, however long that takes; handlers still running a second after
// they were cut short are named in a warning. It reports ctx's error when
// handlers had to be cut short. Every stop returns once the first one has
// finished, and reports its result.
func (s *Server) Shutdown(ctx context.Context) error {
	s.stop(ctx)
	return errors.Join(s.cut, s.err)
}

// Close is Shutdown without letting any handler finish on its own. Cutting
// handlers short is what it is for, so only closing errors are reported.
// A stop already under way is not hurried: Close waits for it, grace and
// all.
func (s *Server) Close() error {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.stop(ctx)
	return s.err
}

func (s *Server) stop(ctx context.Context) {
	s.once.Do(func() {
		s.mu.Lock()
		s.stopping = true
		if len(s.running) == 0 {
			close(s.idle)
		}
		s.mu.Unlock()
		err := s.srv.Shutdown(ctx)
		cut := ctx.Err() != nil && errors.Is(err, ctx.Err())
		if !cut {
			s.err = err
			// Hijacked connections are not the server's to wait for, but
			// their handlers are still counted here.
			select {
			case <-s.idle:
			default:
				select {
				case <-s.idle:
				case <-ctx.Done():
					cut = true
				}
			}
		}
		if cut {
			s.cut = ctx.Err()
			// Connections first: a handler that answers once its context
			// is cancelled then cannot reach its caller as finished.
			s.err = errors.Join(s.err, s.srv.Close())
			s.abort()
			s.awaitCut()
		}
		<-s.idle
		s.abort()
		close(s.stopped)
	})
	<-s.stopped
}

// handle runs next unless a stop has begun. A request can still reach this
// point after the stop closed the listeners; it is refused, so that nothing
// joins the handlers a stop is already waiting for.
func (s *Server) handle(next http.Handler, w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		w.Header().Set("Connection", "close")
		http.Error(w, "server is stopping", http.StatusServiceUnavailable)
		return
	}
	id := s.next
	s.next++
	// The path without its query, which can carry credentials.
	s.running[id] = request{method: r.Method, path: r.URL.Path, since: time.Now()}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.running, id)
		if s.stopping && len(s.running) == 0 {
			close(s.idle)
		}
		s.mu.Unlock()
	}()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	defer context.AfterFunc(s.force, cancel)()
	next.ServeHTTP(w, r.WithContext(ctx))
}

// awaitCut waits a moment for the handlers a stop has cut short. Those
// still running after it ignore their cancelled contexts, and the stop
// waits for them however long they take, so it says which they are.
func (s *Server) awaitCut() {
	timer := time.NewTimer(stuckReportAfter)
	defer timer.Stop()
	select {
	case <-s.idle:
		return
	case <-timer.C:
	}
	s.mu.Lock()
	addr := s.addr
	running := make([]request, 0, len(s.running))
	for _, r := range s.running {
		running = append(running, r)
	}
	s.mu.Unlock()
	slices.SortFunc(running, func(a, b request) int { return a.since.Compare(b.since) })
	if len(running) == 0 {
		return
	}
	names := make([]string, 0, min(len(running), 10))
	for _, r := range running[:min(len(running), 10)] {
		names = append(names, fmt.Sprintf("%s %s (running %s)", r.method, r.path, time.Since(r.since).Round(time.Millisecond)))
	}
	if len(running) > len(names) {
		names = append(names, fmt.Sprintf("and %d more", len(running)-len(names)))
	}
	slog.Warn(fmt.Sprintf("httpdrain: server on %s: %d request(s) still running after their contexts were cancelled; stopping waits for them: %s", addr, len(running), strings.Join(names, ", ")), "server", addr, "requests", len(running))
}
