package httpapi

import (
	"context"
	"net/http"
	"testing"
	"time"

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
