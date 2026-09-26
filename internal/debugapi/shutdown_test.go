package debugapi

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/steve/internal/channel/feishu"
)

// heldGateway keeps every injected message until it is released, and does
// not look at any context while it waits.
type heldGateway struct {
	fakeGateway
	entered chan struct{}
	release chan struct{}
}

func (g *heldGateway) HandleMessage(feishu.InboundMessage) error {
	close(g.entered)
	<-g.release
	return nil
}

// Serve's return is what the application waits on before it closes what
// the gateway writes to, so it may not come while a request is still being
// handled.
func TestServeReturnsOnlyOnceInFlightRequestsHaveFinished(t *testing.T) {
	gw := &heldGateway{entered: make(chan struct{}), release: make(chan struct{})}
	bound := make(chan string, 1)
	listen := func(network, address string) (net.Listener, error) {
		listener, err := net.Listen(network, address)
		if err == nil {
			bound <- listener.Addr().String()
		}
		return listener, err
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- serve(ctx, "127.0.0.1:0", gw, Defaults{ChatID: "oc_chat"}, listen) }()
	url := "http://" + <-bound
	answered := make(chan int, 1)
	go func() {
		resp, err := http.Post(url+"/message", "application/json", strings.NewReader(`{"text":"hi"}`))
		if err != nil {
			answered <- 0
			return
		}
		resp.Body.Close()
		answered <- resp.StatusCode
	}()
	<-gw.entered
	cancel()
	select {
	case err := <-served:
		t.Fatalf("Serve returned (%v) while a request was still being handled", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(gw.release)
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	if code := <-answered; code != http.StatusAccepted {
		t.Fatalf("the request in flight was answered with %d", code)
	}
}
