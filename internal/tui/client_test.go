package tui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientRedirectPolicyCoversSnapshotAndStream(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "snapshot"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			var redirected atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/redirected" {
					redirected.Store(true)
					return
				}
				http.Redirect(writer, request, "/redirected", http.StatusTemporaryRedirect)
			}))
			defer server.Close()
			var checked bool
			model := New(Config{
				URL: server.URL,
				CheckRedirect: func(request *http.Request, via []*http.Request) error {
					checked = true
					return http.ErrUseLastResponse
				},
			})
			if stream {
				if err := model.stream(context.Background(), func() {}); err == nil {
					t.Fatal("redirect counted as successful stream")
				}
			} else {
				model.refresh(context.Background())
				if model.lastErr == "" {
					t.Fatal("redirect counted as successful snapshot")
				}
			}
			if !checked || redirected.Load() {
				t.Fatal("client did not apply redirect policy")
			}
		})
	}
}

// The hub ends the stream of a client that fell behind; top reconnects and
// is poked to redraw, which re-reads the snapshot.
func TestWatchReconnectsWhenTheHubEndsTheStream(t *testing.T) {
	var connections atomic.Int32
	second := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		if connections.Add(1) == 2 {
			close(second)
		}
		writer.WriteHeader(http.StatusOK)
		// Every stream ends as soon as it opens.
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var pokes atomic.Int32
	model := New(Config{URL: server.URL})
	go model.watch(ctx, func() { pokes.Add(1) })
	select {
	case <-second:
	case <-time.After(10 * time.Second):
		t.Fatal("top did not reconnect after the hub ended its stream")
	}
	if pokes.Load() < 2 {
		t.Fatalf("pokes = %d, want a redraw for the open stream and for its end", pokes.Load())
	}
}
