package httpdrain

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// started serves handler on a loopback port and returns the server, its URL
// and what Serve returned, once it has.
func started(t *testing.T, handler http.Handler) (*Server, string, <-chan error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := New(&http.Server{Handler: handler})
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return server, "http://" + listener.Addr().String(), served
}

// call issues GET url and reports the status, or 0 when no answer came.
func call(url string) <-chan int {
	answered := make(chan int, 1)
	go func() {
		resp, err := http.Get(url)
		if err != nil {
			answered <- 0
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		answered <- resp.StatusCode
	}()
	return answered
}

func notYet[T any](t *testing.T, ch <-chan T, what string) {
	t.Helper()
	select {
	case v := <-ch:
		t.Fatalf("%s (%v) while a handler was still running", what, v)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestShutdownLetsAHandlerFinishWithinTheGrace(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	server, url, served := started(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		if r.Context().Err() != nil {
			t.Error("a handler finishing within the grace saw its context cancelled")
		}
		w.WriteHeader(http.StatusTeapot)
	}))
	answered := call(url)
	<-entered
	stopped := make(chan error, 1)
	go func() { stopped <- server.Shutdown(t.Context()) }()
	notYet(t, stopped, "Shutdown returned")
	notYet(t, served, "Serve returned")
	close(release)
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	if code := <-answered; code != http.StatusTeapot {
		t.Fatalf("the request in flight was answered with %d", code)
	}
}

func TestShutdownPastItsDeadlineCancelsHandlersAndWaitsForThem(t *testing.T) {
	entered, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	server, url, served := started(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		close(cancelled)
		<-release
	}))
	answered := call(url)
	<-entered
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- server.Shutdown(ctx) }()
	<-cancelled
	notYet(t, stopped, "Shutdown returned")
	notYet(t, served, "Serve returned")
	close(release)
	if err := <-stopped; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown past its deadline returned %v", err)
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	if code := <-answered; code == http.StatusOK {
		t.Fatal("a request cut short was answered as if it had finished")
	}
}

func TestCloseWaitsForAHijackedHandler(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	server, url, _ := started(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		close(entered)
		<-r.Context().Done()
		<-release
	}))
	call(url)
	<-entered
	stopped := make(chan error, 1)
	go func() { stopped <- server.Close() }()
	notYet(t, stopped, "Close returned")
	close(release)
	if err := <-stopped; err != nil {
		t.Fatalf("Close returned %v", err)
	}
}

func TestStopBeforeServeReleasesTheListener(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := New(&http.Server{Handler: http.NotFoundHandler()})
	if err := server.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := server.Serve(listener); err != nil {
		t.Fatal(err)
	}
	if _, err := listener.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("listener still open after Serve returned: %v", err)
	}
}

type failingListener struct{ net.Listener }

var errAccept = errors.New("accept failed")

func (failingListener) Accept() (net.Conn, error) { return nil, errAccept }

func TestServeThatFailsStopsTheServer(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := New(&http.Server{Handler: http.NotFoundHandler()})
	if err := server.Serve(failingListener{listener}); !errors.Is(err, errAccept) {
		t.Fatalf("Serve returned %v", err)
	}
	select {
	case <-server.stopped:
	default:
		t.Fatal("Serve failed without stopping the server")
	}
}

func TestRequestReachingAStoppedServerIsRefused(t *testing.T) {
	server := New(&http.Server{})
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	ran := false
	recorder := httptest.NewRecorder()
	server.handle(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { ran = true }), recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if ran || recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("a request after the stop ran=%v status=%d", ran, recorder.Code)
	}
}
