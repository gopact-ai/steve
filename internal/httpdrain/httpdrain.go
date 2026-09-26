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
	"net"
	"net/http"
	"sync"
)

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
	running  int
	// idle is closed once a stop has begun and no handler is running.
	idle chan struct{}

	once    sync.Once
	stopped chan struct{}
	err     error
}

func New(srv *http.Server) *Server {
	force, abort := context.WithCancel(context.Background())
	return &Server{srv: srv, force: force, abort: abort, idle: make(chan struct{}), stopped: make(chan struct{})}
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
// return. It reports ctx's error when handlers had to be cut short. Every
// call returns the first call's result once that stop has finished.
func (s *Server) Shutdown(ctx context.Context) error {
	s.once.Do(func() {
		s.mu.Lock()
		s.stopping = true
		if s.running == 0 {
			close(s.idle)
		}
		s.mu.Unlock()
		err := s.srv.Shutdown(ctx)
		if err == nil {
			// Hijacked connections are not the server's to wait for, but
			// their handlers are still counted here.
			select {
			case <-s.idle:
			default:
				select {
				case <-s.idle:
				case <-ctx.Done():
					err = ctx.Err()
				}
			}
		}
		if err != nil {
			s.abort()
			err = errors.Join(err, s.srv.Close())
		}
		<-s.idle
		s.abort()
		s.err = err
		close(s.stopped)
	})
	<-s.stopped
	return s.err
}

// Close is Shutdown without letting any handler finish on its own.
func (s *Server) Close() error {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return s.Shutdown(ctx)
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
	s.running++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.running--
		if s.stopping && s.running == 0 {
			close(s.idle)
		}
		s.mu.Unlock()
	}()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	defer context.AfterFunc(s.force, cancel)()
	next.ServeHTTP(w, r.WithContext(ctx))
}
