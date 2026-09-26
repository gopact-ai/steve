package httpdrain

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
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

// within waits for ch, failing with what if it has not delivered in time:
// a stop that never reaches a handler shows up as that, not as a hang.
func within[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatalf("%s", what)
		panic("unreachable")
	}
}

// releaser returns a channel handlers wait on and the function that closes
// it, which also runs when the test ends, so that a failed test does not
// leave its server waiting on a handler at cleanup.
func releaser(t *testing.T) (<-chan struct{}, func()) {
	release := make(chan struct{})
	var once sync.Once
	let := func() { once.Do(func() { close(release) }) }
	t.Cleanup(let)
	return release, let
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
	entered := make(chan struct{})
	var release <-chan struct{}
	server, url, served := started(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		if r.Context().Err() != nil {
			t.Error("a handler finishing within the grace saw its context cancelled")
		}
		w.WriteHeader(http.StatusTeapot)
	}))
	release, let := releaser(t)
	answered := call(url)
	within(t, entered, "the request never reached its handler")
	stopped := make(chan error, 1)
	go func() { stopped <- server.Shutdown(t.Context()) }()
	notYet(t, stopped, "Shutdown returned")
	notYet(t, served, "Serve returned")
	let()
	if err := within(t, stopped, "Shutdown did not return once its handler had"); err != nil {
		t.Fatal(err)
	}
	if err := within(t, served, "Serve did not return once the stop had finished"); err != nil {
		t.Fatal(err)
	}
	if code := within(t, answered, "the request in flight got no answer"); code != http.StatusTeapot {
		t.Fatalf("the request in flight was answered with %d", code)
	}
}

func TestShutdownPastItsDeadlineCancelsHandlersAndWaitsForThem(t *testing.T) {
	entered, cancelled := make(chan struct{}), make(chan struct{})
	var release <-chan struct{}
	server, url, served := started(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-r.Context().Done():
			close(cancelled)
		case <-release:
			return
		}
		<-release
	}))
	release, let := releaser(t)
	answered := call(url)
	within(t, entered, "the request never reached its handler")
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- server.Shutdown(ctx) }()
	within(t, cancelled, "the handler never saw its context cancelled once the grace was over")
	notYet(t, stopped, "Shutdown returned")
	notYet(t, served, "Serve returned")
	let()
	if err := within(t, stopped, "Shutdown did not return once its handler had"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown past its deadline returned %v", err)
	}
	if err := within(t, served, "Serve did not return once the stop had finished"); err != nil {
		t.Fatal(err)
	}
	if code := within(t, answered, "the request cut short got no answer"); code == http.StatusOK {
		t.Fatal("a request cut short was answered as if it had finished")
	}
}

func TestCloseWaitsForAHijackedHandler(t *testing.T) {
	entered, cancelled := make(chan struct{}), make(chan struct{})
	var release <-chan struct{}
	server, url, _ := started(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		close(entered)
		select {
		case <-r.Context().Done():
			close(cancelled)
		case <-release:
			return
		}
		<-release
	}))
	release, let := releaser(t)
	call(url)
	within(t, entered, "the request never reached its handler")
	stopped := make(chan error, 1)
	go func() { stopped <- server.Close() }()
	within(t, cancelled, "a hijacked handler never saw its context cancelled by Close")
	notYet(t, stopped, "Close returned")
	let()
	if err := within(t, stopped, "Close did not return once its handler had"); err != nil {
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
