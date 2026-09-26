package httpapi

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/consoleapi"
	"github.com/gopact-ai/steve/internal/readmodel"
)

func TestShutdownCancelsStreamsAndWaitsForRequestHandlers(t *testing.T) {
	s, err := NewServer(readmodel.New(readmodel.Sources{}), ServerConfig{Addr: "127.0.0.1:0", Token: "test-token"})
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- s.Serve() }()
	req, _ := http.NewRequest("GET", s.URL()+"/events", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("SSE prevented graceful shutdown: %v", err)
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	if _, err := client.Get(s.URL() + "/state"); err == nil {
		t.Fatal("shutdown accepted another request")
	}
}

// heldCoordination answers the coordination view only once released, and
// does not look at the request's context while it waits.
type heldCoordination struct {
	consoleapi.CoordinationService
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *heldCoordination) Coordination(context.Context) (consoleapi.CoordinationView, error) {
	close(c.entered)
	<-c.release
	return consoleapi.CoordinationView{}, nil
}

func (c *heldCoordination) let() { c.once.Do(func() { close(c.release) }) }

// What closes after the console relies on its handlers having returned, so a
// shutdown that runs out of time still does not return before they have.
func TestShutdownPastItsDeadlineWaitsForHandlersThatIgnoreCancellation(t *testing.T) {
	s, err := NewServer(readmodel.New(readmodel.Sources{}), ServerConfig{Addr: "127.0.0.1:0", Token: "test-token"})
	if err != nil {
		t.Fatal(err)
	}
	held := &heldCoordination{entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(held.let)
	s.SetCoordination(held)
	served := make(chan error, 1)
	go func() { served <- s.Serve() }()
	answered := make(chan struct{})
	go func() {
		defer close(answered)
		req, _ := http.NewRequest("GET", s.URL()+"/console/coordination", nil)
		req.Header.Set("Authorization", "Bearer test-token")
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	<-held.entered
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- s.Shutdown(ctx) }()
	select {
	case err := <-stopped:
		t.Fatalf("Shutdown returned (%v) while a handler was still running", err)
	case <-time.After(300 * time.Millisecond):
	}
	select {
	case err := <-served:
		t.Fatalf("Serve returned (%v) while a handler was still running", err)
	default:
	}
	held.let()
	if err := <-stopped; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown past its deadline returned %v", err)
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	<-answered
}
