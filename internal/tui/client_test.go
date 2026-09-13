package tui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
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
